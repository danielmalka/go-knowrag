// Package lock keeps two ingestion runs off the same scope, using a local OS advisory lock
// (flock(2), non-blocking) keyed by Qdrant endpoint + collection + tenant.
//
// It exists because ADR-006 is insert-then-prune: run A's prune deletes points run B has just
// written, and the prefetch snapshot both runs take becomes a view of a state that no longer holds.
// The damage is detected afterwards by the integrity predicate; it is not prevented by it.
//
// The lock file lives at os.UserCacheDir()/knowrag/locks/<sha256 of the scope>.lock and is left on
// disk after Release. Its presence is not the lock — the flock the kernel holds on an open file
// descriptor is. That is the whole reason this is not a sentinel file: the kernel drops the lock
// when the holding process dies, whatever the cause, so a crash never leaves something for an
// operator to remove by hand (ADR-005 §5). That directory has to be on a filesystem whose flock
// excludes across processes, so TryAcquire refuses to run on 9p, NFS or FUSE rather than hand out
// a lock that does not lock.
//
// # Endpoint normalisation, and what it does not buy
//
// The endpoint is normalised — trimmed, scheme stripped, port made explicit and checked, host
// lowercased, IP literals canonicalised, names resolved to their lowest address — before it enters
// the key. Without that, localhost:6334, 127.0.0.1:6334 and the mesh IP can be one Qdrant reached
// three ways and produce three different locks, and the lock is decorative: it excludes two
// invocations that happened to spell the endpoint identically, and nothing else (ADR-005 §3).
//
// What derivation closes is the spellings that reduce to one address: the scheme, an implicit port,
// letter case, the shape of an IP literal, surrounding whitespace, a name that resolves to that
// address, and the two families' loopback addresses, which are one dual-stack listener. That covers
// the first two spellings ADR-005 §3 names — localhost:6334 and 127.0.0.1:6334 agree, and so does
// [::1]:6334. An endpoint that cannot be reduced is an error rather than a second key.
//
// It does not close the third. A Qdrant reachable both on a forwarded local port and at its mesh
// address gives 127.0.0.1 for one spelling and the mesh IP for the other: two genuinely different
// network paths, and no derivation done on the client can decide they end at the same server. If
// that ever bites, the fix is an operator-configured lock identity — one stable name per Qdrant
// server, set in configuration — not more inference here. It is not built, because nothing needs it.
//
// The lookup runs inside every New, so the key inherits whatever the resolver is doing at that
// moment. Split-horizon DNS, a round-robin answer, or an address set that changes between two runs
// hands two processes two keys for one server, and nothing fails while it happens.
//
// # No cross-host exclusion
//
// This lock does not cross machines. Any host with the binary, the ingestion credential and a route
// to Qdrant is physically an executor, and nothing here stops it from running a second ingestion
// over the same scope. The topology says there is one executor; that is an agreement, not a
// guarantee, and the guard against violating it is the host allowlist in the runbook (ADR-005 §8).
package lock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ErrHeld reports that another process is already ingesting this scope. It is a distinct value so a
// caller can tell "someone else is working" — an orderly refusal — from a lock that is broken.
var ErrHeld = errors.New("ingestion lock is held by another process")

// defaultPort is the Qdrant gRPC port. QDRANT_ENDPOINT is documented as host:6334 in
// internal/config, so an endpoint written without a port means that one.
const defaultPort = "6334"

// resolveTimeout bounds the name lookup New performs. It is applied inside New rather than left to
// the caller because an unbounded DNS call in a constructor is a hang nobody expects there.
const resolveTimeout = 5 * time.Second

// Lock is exclusive access to one ingestion scope.
type Lock interface {
	TryAcquire() error
	Release() error
}

// FileLock is a Lock backed by flock(2) on a file under the user cache directory.
//
// One value belongs to one goroutine: the open file is read and written without synchronisation, so
// a caller that needs the lock in two places constructs one FileLock per scope rather than sharing
// one. There is deliberately no mutex — it would advertise a sharing this type does not support.
type FileLock struct {
	path string
	file *os.File // non-nil only while this value holds the lock
}

var _ Lock = (*FileLock)(nil)

// New derives the lock file path for one ingestion scope. It resolves the endpoint, so it can do
// network I/O and can fail.
func New(ctx context.Context, endpoint, collection, tenant string) (*FileLock, error) {
	canonical, err := canonicalEndpoint(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locating the user cache directory for the ingestion lock: %w", err)
	}
	return &FileLock{path: filepath.Join(cache, "knowrag", "locks", scopeKey(canonical, collection, tenant)+".lock")}, nil
}

// scopeKey hashes the components of one scope so that no two different scopes can produce one
// digest. Each is length-prefixed rather than joined by a separator: collection and tenant are
// operator free text and nothing rejects a "|" in either, so a plain "|" between them makes
// ("a|b", "c") and ("a", "b|c") the same key — a run refused for a scope no one else is holding.
// Rejecting "|" in the values would be the other fix, and it constrains the operator to suit the
// hasher.
func scopeKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s", len(p), p) // writing to a hash.Hash never fails
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Path is the lock file this value acquires. Exported so a test can assert that two spellings of
// one endpoint land on one file.
func (l *FileLock) Path() string { return l.path }

// TryAcquire takes the lock, or returns ErrHeld immediately if another process holds it. It never
// blocks and never polls: a run that has to wait is a run that should tell the operator why.
func (l *FileLock) TryAcquire() error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the ingestion lock directory: %w", err)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return fmt.Errorf("checking which filesystem holds %s: %w", dir, err)
	}
	if err := checkLockFS(dir, int64(stat.Type)); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- the path is a hash of the scope, derived here, not caller-supplied.
	if err != nil {
		return fmt.Errorf("opening the ingestion lock file: %w", err)
	}
	// EAGAIN and EWOULDBLOCK are the same value on Linux, so one comparison covers both spellings.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrHeld
		}
		return fmt.Errorf("locking %s: %w", l.path, err)
	}
	l.file = f
	return nil
}

// Release drops the lock. The lock file itself stays on disk on purpose — the next reader will want
// to delete it; it is not the lock, and removing it would race a process that has the file open.
// Releasing a lock this value does not hold is a no-op, so `defer Release()` is always safe.
func (l *FileLock) Release() error {
	if l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	// Joined rather than first-one-wins: a failed unlock and a failed close are two different faults,
	// and whichever one gets dropped is the one nobody ever gets to debug.
	if err := errors.Join(unix.Flock(int(f.Fd()), unix.LOCK_UN), f.Close()); err != nil {
		return fmt.Errorf("releasing %s: %w", l.path, err)
	}
	return nil
}

// unreliableLockFS are the filesystems where flock(2) has historically not excluded across
// processes. os.UserCacheDir follows XDG_CACHE_HOME, so an operator can land the lock directory on
// one without meaning to: on this deployment a Windows drive under /mnt/c is 9p, and two runs there
// would each be told they hold the lock. Today's default (~/.cache on ext4) is fine; nothing keeps
// it that way, and every test in this package points XDG_CACHE_HOME at a temporary directory, so
// the suite structurally cannot notice.
var unreliableLockFS = map[int64]string{
	unix.V9FS_MAGIC:       "9p",
	unix.NFS_SUPER_MAGIC:  "NFS",
	unix.FUSE_SUPER_MAGIC: "FUSE",
}

// checkLockFS refuses a lock directory on a filesystem whose flock does not exclude. A lock that
// cannot exclude must not report success — silently not locking is the failure this whole package
// exists to prevent, and it corrupts the index with no error anywhere.
//
// It takes the magic number rather than the path so a test can state the case instead of needing a
// mount it has no privilege to make.
func checkLockFS(dir string, magic int64) error {
	name, unreliable := unreliableLockFS[magic]
	if !unreliable {
		return nil
	}
	return fmt.Errorf(
		"the ingestion lock directory %s is on %s (filesystem type %#x), where flock does not "+
			"reliably exclude other processes: point XDG_CACHE_HOME at a path on a local Linux "+
			"filesystem, because a lock taken here would let two ingestions run at once", dir, name, magic)
}

// canonicalEndpoint reduces an endpoint to one host:port per Qdrant server it can reach.
func canonicalEndpoint(ctx context.Context, endpoint string) (string, error) {
	// Trimmed before anything is parsed. config.overrideFromEnv assigns the environment value
	// verbatim, and a .env authored on a Windows drive delivers QDRANT_ENDPOINT with a trailing \r —
	// which SplitHostPort happily hands back as part of the port. Untrimmed, that is one server with
	// two lock files and no complaint from anyone.
	raw := strings.TrimSpace(endpoint)
	if _, rest, found := strings.Cut(raw, "://"); found {
		raw = rest
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		// Either no port, or a bare IPv6 literal that SplitHostPort reads as "too many colons".
		// Both mean the endpoint named a host and left the port implicit.
		host, port = strings.Trim(raw, "[]"), defaultPort
	} else if port, err = canonicalPort(port); err != nil {
		// Same refusal as the DNS failure below, for the same reason. Nothing is repaired here: an
		// endpoint carrying "/collections" or an unparseable port is a mistake with more than one
		// plausible correction, and hashing a guess is how the lock ends up keyed on a server nobody
		// is talking to.
		return "", fmt.Errorf("normalising %q for the ingestion lock key: %w", endpoint, err)
	}
	host = strings.ToLower(host)

	if addr, err := netip.ParseAddr(host); err == nil {
		return net.JoinHostPort(canonicalHost(addr), port), nil
	}

	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		// No falling back to the raw string. A fallback quietly rebuilds the decorative lock the
		// normalisation exists to prevent, and an endpoint that will not resolve is one the
		// ingestion is about to fail on anyway — failing here says so earlier and more clearly.
		return "", fmt.Errorf("resolving %q for the ingestion lock key: %w", endpoint, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolving %q for the ingestion lock key: no addresses", endpoint)
	}
	// The lowest address, not the whole set: localhost resolves to {127.0.0.1, ::1} while the
	// literal 127.0.0.1 resolves to one address, so joining the set would give one server two keys —
	// the exact failure ADR-005 §3 names. Taking the lowest makes both spellings agree.
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	return net.JoinHostPort(canonicalHost(slices.MinFunc(addrs, netip.Addr.Compare)), port), nil
}

// loopbackHost is the token 127.0.0.1 and ::1 both reduce to.
const loopbackHost = "loopback"

// canonicalHost is the host half of the key. It collapses the two families' loopback addresses onto
// one token, because a Qdrant bound dual-stack — the default — is one listener answering both, and
// internal/store's grpcHost dials either spelling happily. Left apart, 127.0.0.1:6334 and
// [::1]:6334 are two lock files and two runs that both connect and both write.
//
// It is the only pair a client can settle. A server bound on 0.0.0.0 and reached by two routable
// addresses is one listener too, and nothing here can know that. Nor is even this pair provable —
// two servers can bind them separately with IPV6_V6ONLY — but that mistake refuses a run that could
// have proceeded, while the one it replaces lets two runs write at once.
//
// Only those two addresses, not all of 127.0.0.0/8: 127.0.0.2 is a bind address of its own, and
// this repo already uses it as a distinct endpoint.
func canonicalHost(addr netip.Addr) string {
	host := addr.Unmap().String()
	if host == "127.0.0.1" || host == "::1" {
		return loopbackHost
	}
	return host
}

// canonicalPort checks what SplitHostPort called a port and returns its one spelling. SplitHostPort
// takes whatever follows the last colon, so "6334\r", "6334 ", "6334/collections" and "06334" all
// arrive here; the first three are malformed and the last is 6334 written differently. Re-formatting
// the parsed number is what makes the last one land on the same lock file.
func canonicalPort(port string) (string, error) {
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", fmt.Errorf("%q is not a port number in 1-65535", port)
	}
	return strconv.FormatUint(n, 10), nil
}
