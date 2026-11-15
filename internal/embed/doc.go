// Package embed turns chunk text into the EmbeddingGemma 2 dense vectors the rest of the system stores
// and searches on.
//
// Two things about this package are load-bearing and are not implementation taste:
//
//   - It never loads a model. Loading takes seconds even for this small model (and minutes the first
//     time, which downloads the weights), so the runtime is a separate service that owns the model and
//     its GPU memory (scripts/embedder-service); everything here talks to something that is already up.
//   - It never repairs a vector. This is the boundary where an untrusted backend's output becomes
//     trusted payload data (PRD-contrato §2.4: the confirmed embedder config feeds point_hash), so
//     a response that violates an invariant is an error, not something to clean up. What to do
//     about that error is the pipeline's call (S06a), not this package's.
package embed
