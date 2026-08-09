// Package schema holds the project's identity constants and the deterministic point-ID formula.
package schema

import (
	"strconv"

	"github.com/google/uuid"
)

// NamespaceKnowrag is the fixed UUID namespace for this project's point IDs, as a literal string.
// Generated once (2026-08-08) and MUST NEVER change: changing it invalidates every point ID
// already written across all three collections. It is a const, not a var, precisely so no package
// can reassign it at runtime and silently orphan every point already in Qdrant.
const NamespaceKnowrag = "34bf96b9-3a72-4c17-8711-9addd5d8e946"

// namespaceKnowrag is NamespaceKnowrag parsed once. Unexported: the parsed form is an
// implementation detail of PointID, and an exported var would reintroduce the mutability the
// const above exists to remove.
var namespaceKnowrag = uuid.MustParse(NamespaceKnowrag)

// BGEM3Revision is the pinned, immutable HuggingFace commit revision of BAAI/bge-m3.
// Confirmed 2026-08-08: current HEAD of BAAI/bge-m3 on Hugging Face; the weights repo has had
// no new commit in about two years. Changing it requires reindexing all three collections
// (PRD-contrato §2.3b).
const BGEM3Revision = "5617a9f61b028005a4858fdac845db406aefb181"

// ChunkerVersion identifies the chunking algorithm + config version; feeds point_hash (PRD-contrato §2.4).
const ChunkerVersion = "v1"

// PointID computes the deterministic Qdrant point ID for a chunk:
// UUIDv5(NamespaceKnowrag, tenantID + ":" + uid + ":" + decimal(chunkIndex)), per PRD-contrato §2.3b.
//
// uid is a uuid.UUID, not a string, and that is load-bearing — do not "simplify" it back.
// A string parameter made the hashed name ambiguous in two ways at once:
//   - Canonicalization was a matter of discipline. uuid.Parse accepts five spellings of the same
//     UUID (canonical, upper-case, unhyphenated, "urn:uuid:…", "{…}"); hashing the raw string gave
//     the same logical note five different point IDs.
//   - The joined name was not uniquely decodable. PointID("interno", "urn:uuid:<u>", 3) and
//     PointID("interno:urn:uuid", "<u>", 3) built the identical name and therefore the identical
//     point ID — two distinct entries mapping to one point, which is exactly the silent overwrite
//     that putting tenant_id in the point ID exists to prevent.
//
// uuid.UUID.String() is always the canonical 36-character form, which fixes both: the name is
// parseable from the right without ambiguity even when tenantID itself contains ':'. Read the
// suffix after the last ':' as the decimal index (no ':' in a decimal), the 36 characters before
// that ':' as the uid (no ':' in a canonical UUID), and everything before the preceding ':' as the
// tenant. One name, one triple.
//
// Precondition: chunkIndex >= 0. PointID returns no error, deliberately. With a uuid.UUID there is
// no malformed uid left to reject, so the only rejectable input would be a negative chunkIndex —
// and an error return that can never fire in practice buys nothing but an unreachable branch at
// every call site. A negative index is a caller loop bug, not a collision risk: "-1" still hashes
// to a distinct, deterministic ID. Nor would an unsigned parameter help, since converting a
// negative int to uint wraps silently, turning a visible bug into an invisible one.
func PointID(tenantID string, uid uuid.UUID, chunkIndex int) uuid.UUID {
	name := tenantID + ":" + uid.String() + ":" + strconv.Itoa(chunkIndex)
	return uuid.NewSHA1(namespaceKnowrag, []byte(name))
}
