package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verifyDeployScript and deployCompose are the two files scripts/verify-deploy.sh's header points
// at once this test exists: the script it checks, and the real compose file its positive case runs
// against. Paths are relative to this package, the same way eval_test.go's evalGateScript points at
// scripts/ci/eval-gate.sh.
const (
	verifyDeployScript = "../../scripts/verify-deploy.sh"
	deployCompose      = "../../deploy/docker-compose.yml"
)

// The four fields below pass every one of scripts/verify-deploy.sh's checks, so each case in
// TestVerifyDeploy_RejectsBrokenCopies overrides exactly one and leaves the rest at these values —
// isolating which check fired. None of this is a real credential: this repository is public, and
// the rule for fixtures here is the same as for everything else tracked in it.
const (
	validImage  = "qdrant/qdrant:v1.19.0"
	validPort   = `- "${QDRANT_BIND_ADDR:?set it}:6334:6334"`
	validAPIKey = "QDRANT__SERVICE__API_KEY: ${QDRANT_API_KEY:?set it}" // #nosec G101 -- env reference, not a credential
	validROKey  = "QDRANT__SERVICE__READ_ONLY_API_KEY: ${QDRANT_READ_ONLY_API_KEY:?set it}"
)

// composeFixture assembles a minimal but structurally faithful docker-compose.yml: one service, one
// image line, one ports entry, two environment lines. An empty argument omits its line entirely
// rather than writing it blank, because a blank image value is still a line the script's grep
// matches — omitting the line is how the "missing" cases below are built.
func composeFixture(image, port, apiKey, roKey string) string {
	var b strings.Builder
	b.WriteString("services:\n  qdrant:\n")
	if image != "" {
		b.WriteString("    image: " + image + "\n")
	}
	b.WriteString("    environment:\n")
	if apiKey != "" {
		b.WriteString("      " + apiKey + "\n")
	}
	if roKey != "" {
		b.WriteString("      " + roKey + "\n")
	}
	b.WriteString("    ports:\n")
	if port != "" {
		b.WriteString("      " + port + "\n")
	}
	return b.String()
}

// runVerifyDeploy writes compose to a fresh directory as docker-compose.yml, optionally writes env
// alongside it as .env, and runs scripts/verify-deploy.sh against that path.
//
// The directory matters for check 5: the script reads "$(dirname "$compose")/.env", never the
// repository's deploy/.env, so a fixture exercising that check needs its own .env sitting next to
// its own compose file. A nil env leaves the directory without one, which is how the "not checked"
// case is built.
func runVerifyDeploy(t *testing.T, compose string, env *string) (exitCode int, output string) {
	t.Helper()

	dir := t.TempDir()
	composePath := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o600); err != nil {
		t.Fatalf("writing fixture compose file: %v", err)
	}
	if env != nil {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(*env), 0o600); err != nil {
			t.Fatalf("writing fixture .env file: %v", err)
		}
	}

	cmd := exec.Command("bash", verifyDeployScript, composePath) // #nosec G204 -- fixed script, generated fixture path
	out, err := cmd.CombinedOutput()
	output = string(out)
	if err == nil {
		return 0, output
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), output
	}
	t.Fatalf("running %s: %v\noutput:\n%s", verifyDeployScript, err, output)
	return -1, output
}

// TestVerifyDeploy_RejectsBrokenCopies is the test scripts/verify-deploy.sh's header comment names:
// one deliberately broken compose file per failure path in the script, confirming each actually
// fails rather than silently accepting the mistake it claims to catch.
func TestVerifyDeploy_RejectsBrokenCopies(t *testing.T) {
	cases := []struct {
		name                       string
		image, port, apiKey, roKey string
		wantSubstr                 string
	}{
		// Check 1: the image pin.
		{"image :latest", "qdrant/qdrant:latest", validPort, validAPIKey, validROKey,
			"never :latest and never another minor"},
		{"image on another minor", "qdrant/qdrant:v1.18.5", validPort, validAPIKey, validROKey,
			"never :latest and never another minor"},
		{"image with no patch digits", "qdrant/qdrant:v1.19.x", validPort, validAPIKey, validROKey,
			"pin an exact patch on the v1.19.x line"},
		{"image line absent", "", validPort, validAPIKey, validROKey,
			"no image is declared"},

		// Check 2: every published port names one interface.
		{"port with two-field mapping", validImage, `- "6334:6334"`, validAPIKey, validROKey,
			"publishes on every interface"},
		{"port bound to 0.0.0.0", validImage, `- "0.0.0.0:6334:6334"`, validAPIKey, validROKey,
			"binds the wildcard"},
		{"port bound to a literal address", validImage, `- "203.0.113.5:6334:6334"`, validAPIKey, validROKey,
			"hardcodes the bind address"},

		// Check 3: the administrative key.
		{"API key absent", validImage, validPort, "", validROKey,
			"QDRANT__SERVICE__API_KEY is not set"},
		{"API key is a literal", validImage, validPort, "QDRANT__SERVICE__API_KEY: admin-key-fixture", validROKey,
			"QDRANT__SERVICE__API_KEY is a literal"},
		{"API key has no :? marker", validImage, validPort, "QDRANT__SERVICE__API_KEY: ${QDRANT_API_KEY}", validROKey,
			"QDRANT__SERVICE__API_KEY has no ':?'"},

		// Check 4: the read-only key, same three ways.
		{"read-only key absent", validImage, validPort, validAPIKey, "",
			"QDRANT__SERVICE__READ_ONLY_API_KEY is not set"},
		{"read-only key is a literal", validImage, validPort, validAPIKey,
			"QDRANT__SERVICE__READ_ONLY_API_KEY: readonly-key-fixture",
			"QDRANT__SERVICE__READ_ONLY_API_KEY is a literal"},
		{"read-only key has no :? marker", validImage, validPort, validAPIKey,
			"QDRANT__SERVICE__READ_ONLY_API_KEY: ${QDRANT_READ_ONLY_API_KEY}",
			"QDRANT__SERVICE__READ_ONLY_API_KEY has no ':?'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compose := composeFixture(tc.image, tc.port, tc.apiKey, tc.roKey)
			code, output := runVerifyDeploy(t, compose, nil)
			if code == 0 {
				t.Fatalf("verify-deploy.sh exited 0 against a fixture broken this way; output:\n%s", output)
			}
			if !strings.Contains(output, tc.wantSubstr) {
				t.Errorf("output did not contain %q; got:\n%s", tc.wantSubstr, output)
			}
		})
	}
}

// TestVerifyDeploy_AcceptsTheValidFixture is the sanity check the table above depends on: every
// case there changes exactly one field away from this baseline. If the baseline itself failed, a
// case could pass for the wrong reason — failing regardless of which field it mutated — and the
// table would still read green.
func TestVerifyDeploy_AcceptsTheValidFixture(t *testing.T) {
	compose := composeFixture(validImage, validPort, validAPIKey, validROKey)
	code, output := runVerifyDeploy(t, compose, nil)
	if code != 0 {
		t.Fatalf("the fixture every case in the table above is a one-field mutation of failed on its own:\n%s", output)
	}
}

// TestVerifyDeploy_KeyPairChecks covers check 5, the one that reads deploy/.env instead of the
// compose file — and the one easiest to exercise against the wrong .env, since the script looks
// specifically at dirname(compose)/.env rather than the repository's.
func TestVerifyDeploy_KeyPairChecks(t *testing.T) {
	compose := composeFixture(validImage, validPort, validAPIKey, validROKey)

	t.Run("both keys equal", func(t *testing.T) {
		env := "QDRANT_API_KEY=shared-key-fixture\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"
		code, output := runVerifyDeploy(t, compose, &env)
		if code == 0 {
			t.Fatalf("verify-deploy.sh accepted an .env with both keys equal; output:\n%s", output)
		}
		if !strings.Contains(output, "sets both keys to the same value") {
			t.Errorf("output did not name the equal-keys failure; got:\n%s", output)
		}
	})

	t.Run("one key empty", func(t *testing.T) {
		env := "QDRANT_API_KEY=admin-key-fixture\nQDRANT_READ_ONLY_API_KEY=\n"
		code, output := runVerifyDeploy(t, compose, &env)
		if code == 0 {
			t.Fatalf("verify-deploy.sh accepted an .env with an empty key; output:\n%s", output)
		}
		if !strings.Contains(output, "leaves QDRANT_API_KEY or QDRANT_READ_ONLY_API_KEY empty") {
			t.Errorf("output did not name the empty-key failure; got:\n%s", output)
		}
	})

	// The two keys are the same *credential* in each of these, and every one of them slipped past an
	// earlier version of check 5 that compared the raw text after the first `=`. They are here as
	// separate cases rather than one because they fail for unrelated reasons, and fixing one says
	// nothing about the others.
	//
	// What makes them bypasses rather than cosmetics is that docker compose resolves each pair to a
	// single value: it strips surrounding quotes, and on a duplicated key it takes the last line. So
	// the deployed Qdrant holds one credential under two names while the script reports success —
	// the exact state check 5 exists to refuse.
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"same key, quoted on one side", "QDRANT_API_KEY=\"shared-key-fixture\"\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		{"same key, single-quoted on one side", "QDRANT_API_KEY='shared-key-fixture'\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		{"same key, admin declared twice", "QDRANT_API_KEY=rotated-away-fixture\nQDRANT_API_KEY=shared-key-fixture\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		// Only the first line carries the carriage return, and that asymmetry is the whole case: with
		// CRLF on both lines the two raw values differ by the same trailing \r and compare equal
		// anyway, so a fixture written that way passes without the stripping it means to exercise. A
		// file half-edited from Windows is what produces the mixed shape.
		{"same key, CRLF on one line only", "QDRANT_API_KEY=shared-key-fixture\r\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		{"same key, export prefix on one side", "export QDRANT_API_KEY=shared-key-fixture\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		{"same key, inline comment on one side", "QDRANT_API_KEY=shared-key-fixture # rotated Tuesday\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		{"same key, quoted and commented on one side", "QDRANT_API_KEY=\"shared-key-fixture\" # rotated Tuesday\nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
		{"same key, trailing whitespace on one side", "QDRANT_API_KEY=shared-key-fixture   \nQDRANT_READ_ONLY_API_KEY=shared-key-fixture\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			code, output := runVerifyDeploy(t, compose, &env)
			if code == 0 {
				t.Fatalf("verify-deploy.sh accepted an .env whose two keys resolve to one credential; output:\n%s", output)
			}
			if !strings.Contains(output, "sets both keys to the same value") {
				t.Errorf("output did not name the equal-keys failure; got:\n%s", output)
			}
		})
	}

	// Genuinely different keys must still pass, and these carry the same decorations as the cases
	// above. Without them every case above is answered by a check that always fails — and a value
	// whose quotes hold a space is what stops the comment-stripping from eating a legitimate key.
	for _, tc := range []struct {
		name string
		env  string
	}{
		{"plain", "QDRANT_API_KEY=admin-key-fixture\nQDRANT_READ_ONLY_API_KEY=readonly-key-fixture\n"},
		{"quoted", "QDRANT_API_KEY=\"admin-key-fixture\"\nQDRANT_READ_ONLY_API_KEY=readonly-key-fixture\n"},
		{"commented", "QDRANT_API_KEY=admin-key-fixture # the privileged one\nQDRANT_READ_ONLY_API_KEY=readonly-key-fixture\n"},
		{"quoted value containing spaces", "QDRANT_API_KEY=\"admin key fixture\"\nQDRANT_READ_ONLY_API_KEY=readonly-key-fixture\n"},
	} {
		t.Run("genuinely different keys pass, "+tc.name, func(t *testing.T) {
			env := tc.env
			code, output := runVerifyDeploy(t, compose, &env)
			if code != 0 {
				t.Fatalf("verify-deploy.sh rejected two genuinely different keys; output:\n%s", output)
			}
		})
	}

	// The one case in this file where a missing check must NOT fail the script: no .env exists next
	// to the fixture, which is CI's own situation (deploy/.env is gitignored), and check 5 is
	// documented to print NOT CHECKED and move on rather than fail closed.
	t.Run("no .env present", func(t *testing.T) {
		code, output := runVerifyDeploy(t, compose, nil)
		if code != 0 {
			t.Fatalf("a compose file otherwise valid failed with no .env present; output:\n%s", output)
		}
		if !strings.Contains(output, "NOT CHECKED") {
			t.Errorf("an absent .env should print NOT CHECKED for the key-pair check, got:\n%s", output)
		}
	})
}

// TestVerifyDeploy_AcceptsTheRealComposeFile is the positive path make verify-deploy already runs
// in CI. It is a regression guard on that command staying green, not new coverage — the fixture
// cases above are what actually exercises each failure branch.
func TestVerifyDeploy_AcceptsTheRealComposeFile(t *testing.T) {
	cmd := exec.Command("bash", verifyDeployScript, deployCompose) // #nosec G204 -- fixed arguments, no fixture involved
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verify-deploy.sh rejected %s:\n%s", deployCompose, out)
	}
}
