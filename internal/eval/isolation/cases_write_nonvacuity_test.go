package isolation

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/ingest"
	"github.com/danielmalka/go-knowrag/internal/vault"
)

// This file is cases_nonvacuity_test.go's twin, and it exists for the same reason: a case that
// returns "" on a clean tree is satisfied just as well by a case whose assertions have all been
// switched off. Every assertion in WritePathTenantCase has a row here that disables it, and each row
// has to drive the case red.
//
// TestEveryCase_IsCoveredByANonVacuityHarness is what stops a future case from escaping both files.

// withWriteProbe swaps the probe the write case builds, and restores it.
func withWriteProbe(t *testing.T, replacement func() (writeRunner, *captureStore)) {
	t.Helper()
	original := newWriteProbe
	newWriteProbe = replacement
	t.Cleanup(func() { newWriteProbe = original })
}

// scopelessProbe is the store you would have if the tenant half of ADR-006 §2's scope were gone:
// reads, writes and deletes all key on the uid alone.
//
// It is the write-path twin of leakingProbe, and what it produces is the loss the tenant half of
// that scope exists to prevent: three tenants writing the same uid land in one bucket, so what a
// tenant reads back and what remains stored under its own uid belong to whoever wrote last.
func scopelessProbe() (writeRunner, *captureStore) {
	store := newCaptureStore()
	store.ignoreScope = true
	return productionIngest{store: store}, store
}

// rewritingWriteProbe logs a rewritten copy of every call, leaving what the store does untouched —
// see captureStore.recordAs. The stored state is the healthy build's; only what the case inspects
// moves.
func rewritingWriteProbe(rewrite func(writeCall) writeCall) func() (writeRunner, *captureStore) {
	return func() (writeRunner, *captureStore) {
		store := newCaptureStore()
		store.recordAs = rewrite
		return productionIngest{store: store}, store
	}
}

// withForeignPayloadTenant is the point set internal/ingest would hand over if ExpectedPoints were
// built from a tenant other than the one the run was asked for (internal/ingest/note.go). The call's
// own scope is untouched, so the point IDs and everything the store does are unchanged: only the
// payload the search filters on names somebody else.
func withForeignPayloadTenant(c writeCall) writeCall {
	points := make([]ingest.Point, len(c.Points))
	for i, p := range c.Points {
		p.Fields = maps.Clone(p.Fields)
		p.Fields[payloadTenantField] = "tenant-outro"
		points[i] = p
	}
	c.Points = points
	return c
}

// withoutCallScope is the call internal/ingest would make if it stopped passing its tenant to the
// store. Nothing the store does changes under it, because the store is told to log the rewrite and
// not to act on it — which is exactly the regression an assertion on stored state cannot see.
func withoutCallScope(c writeCall) writeCall {
	c.TenantID = ""
	return c
}

// TestWriteCase_FailsWhenTheStoreLosesTheTenantScope is the whole point of this file, in the same
// shape TestEveryCase_FailsAgainstALeakingStore has for the read cases.
func TestWriteCase_FailsWhenTheStoreLosesTheTenantScope(t *testing.T) {
	withWriteProbe(t, scopelessProbe)

	detail := WritePathTenantCase().Run(t.Context())
	if detail == "" {
		t.Fatal("the case passed against a store where every tenant's points share one scope, so its " +
			"green on a clean build proves nothing")
	}
	t.Logf("caught: %s", detail)
}

// TestWriteCase_FailsOnARewrittenCall covers the two facts no inspection of stored state can reach.
func TestWriteCase_FailsOnARewrittenCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rewrite func(writeCall) writeCall
	}{
		{"the payload names another tenant", withForeignPayloadTenant},
		{"the call goes out with no scope", withoutCallScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withWriteProbe(t, rewritingWriteProbe(tc.rewrite))
			if detail := WritePathTenantCase().Run(t.Context()); detail == "" {
				t.Fatal("the case passed while every call carried this rewrite; the stored state is " +
					"identical either way, so without inspecting what was handed over the suite " +
					"cannot see it")
			}
		})
	}
}

// silentRunner reaches the store and writes nothing — a dry run, which is a real mode of this
// pipeline (internal/ingest/note.go) and therefore the honest way to produce "nothing was written"
// without inventing a build.
//
// It is the write-path form of "empty for the wrong reason": no foreign tenant appears anywhere,
// because nothing appears anywhere.
type silentRunner struct{ store ingest.Store }

func (r silentRunner) Ingest(ctx context.Context, tenantID string, notes []vault.Note) error {
	d := writeDeps(tenantID, r.store)
	d.DryRun = true
	_, err := ingest.Orchestrate(ctx, d, vault.ScanResult{Vault: writeVault, Notes: notes})
	return err
}

func TestWriteCase_FailsWhenNothingIsWritten(t *testing.T) {
	withWriteProbe(t, func() (writeRunner, *captureStore) {
		store := newCaptureStore()
		return silentRunner{store: store}, store
	})

	if detail := WritePathTenantCase().Run(t.Context()); detail == "" {
		t.Fatal("the case passed against a run that wrote no point at all; its assertions cannot " +
			"tell a correctly scoped ingestion from an ingestion that did not happen")
	}
}

// riggedWriter is a build that mishandles the empty tenant in exactly one way. It is the seam
// WritePathTenantCase's last assertion needs and no store can provide: the refusal happens in
// Deps.Validate (internal/ingest/note.go) before anything is asked of the store, so a hostile store
// cannot move it.
//
// accepting makes the run report success for a tenant it should have refused, having done nothing —
// which is the shape only the "was accepted" half can catch, since a build that accepted the empty
// tenant *and* ingested under it is already caught by the call it makes. touching makes the run
// refuse, but only after a call has gone out, which only the second half catches. One row per half,
// so emptying the assertion fails both.
type riggedWriter struct {
	real      writeRunner
	store     *captureStore
	accepting bool
	touching  bool
}

func (r riggedWriter) Ingest(ctx context.Context, tenantID string, notes []vault.Note) error {
	if tenantID != "" {
		return r.real.Ingest(ctx, tenantID, notes)
	}
	if r.accepting {
		return nil
	}
	if r.touching {
		// The call an unvalidated empty tenant would make: the whole-tenant snapshot, with no scope.
		if _, err := r.store.ScrollTenant(ctx, tenantID); err != nil {
			return err
		}
	}
	return errors.New("rigged: TenantID is empty")
}

func TestWriteCase_FailsWhenTheEmptyTenantIsMishandled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rigged riggedWriter
	}{
		{"reports success for an empty tenant", riggedWriter{accepting: true}},
		{"refuses only after a call reached the store", riggedWriter{touching: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withWriteProbe(t, func() (writeRunner, *captureStore) {
				store := newCaptureStore()
				rigged := tc.rigged
				rigged.store = store
				rigged.real = productionIngest{store: store}
				return rigged, store
			})
			if detail := WritePathTenantCase().Run(t.Context()); detail == "" {
				t.Fatal("the case reported a pass against a build that did not refuse an empty " +
					"tenant_id outright and before any call was made, so its green on a clean build " +
					"says nothing about the refusal")
			}
		})
	}
}

// TestDefaultSuite_UsesTheRealWriteProbe pins that the seam this file relies on is a test seam and
// nothing more: on a normal run the case drives the production ingestion into a store that scopes
// nothing on its own account.
func TestDefaultSuite_UsesTheRealWriteProbe(t *testing.T) {
	runner, store := newWriteProbe()
	if _, ok := runner.(productionIngest); !ok {
		t.Fatalf("the shipped write probe ingests through a %T, not the production path, so the case "+
			"is measuring a stand-in", runner)
	}
	if store.ignoreScope {
		t.Fatal("the shipped write probe collapses every tenant into one scope, so the case is vacuous")
	}
	if store.recordAs != nil {
		t.Fatal("the shipped write probe rewrites the calls it logs, so the case is inspecting " +
			"something other than what internal/ingest asked for")
	}
}
