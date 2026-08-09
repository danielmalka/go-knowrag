// This file exists to be caught. It is the deliberate violation that proves
// TestArch_QdrantClientConfinedToStore is not passing vacuously — it imports the Qdrant client from
// outside internal/store, which is exactly what that test forbids.
//
// It lives under testdata/ so the Go tool ignores it entirely: it is never built, vetted, linted or
// reached by the real-tree walk. Only the fixture test points the checker at this directory.
//
// Do not "fix" this file. Deleting the import is deleting the proof.
package violation

import _ "github.com/qdrant/go-client/qdrant"
