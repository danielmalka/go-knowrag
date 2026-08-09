// Package embed turns chunk text into the BGE-M3 dense and sparse vectors the rest of the system
// stores and searches on.
//
// Two things about this package are load-bearing and are not implementation taste:
//
//   - It never loads a model. ADR-001 §6.2 measured 314,5 s to load BGE-M3 on this host, so the
//     runtime has to be a resident service with the model already in memory; a design that starts a
//     process per query would make the first search of the day take five minutes. Everything here
//     talks to something that is already up.
//   - It never repairs a vector. This is the boundary where an untrusted backend's output becomes
//     trusted payload data (PRD-contrato §2.4: the confirmed embedder config feeds point_hash), so
//     a response that violates an invariant is an error, not something to clean up. What to do
//     about that error is the pipeline's call (S06a), not this package's.
package embed
