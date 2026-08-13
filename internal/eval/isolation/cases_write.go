package isolation

import (
	"context"
	"fmt"
)

// WritePathTenantCase is the one case that attacks the other end of the pipe.
//
// Every other case in this suite proves the search asks a correctly scoped question. The search
// filters on the tenant_id that sits in the payload (internal/retrieval/build.go), and the payload
// is written here — so a perfect filter over a point whose payload names the wrong tenant returns
// one tenant's note to another, with this gate green, because the filter has no way not to trust
// what it filters on.
//
// The error it guards against is silent by construction: the point ID is
// uuid5(tenant_id + uid + chunk_index) (internal/schema/identity.go), so a point written under the
// wrong tenant overwrites nothing and errors nowhere. It is a new, valid, mis-scoped point.
//
// That the tenant really is part of that identity is not asserted here, deliberately.
// internal/schema proves it directly, where a change to the formula is what fails
// (TestPointID_DifferentTenant_ProducesDifferentID); a copy of the claim in this file could be
// driven red by nothing this suite is able to swap, and an assertion no plant can reach is one that
// reads as coverage without being any.
//
// Four independent things are asserted, and all four are needed:
//
//   - every call an ingestion makes carries the run's tenant as its scope — the write-path twin of
//     requestsCarryTenant, and the only assertion that does not depend on what was stored;
//   - every point handed to the store carries that same tenant in its payload. This is a separate
//     fact from the one above and not a restatement of it: internal/store/points.go builds the point
//     ID from the scope argument and the payload from Point.Fields, so at that boundary the two come
//     from different places and a disagreement is a point identified under one tenant and retrievable
//     by another;
//   - after every tenant has ingested the identical uids, each still holds its own point set,
//     carrying its own tenant. That catches two things at once: a prune that lost its tenant scope,
//     which is the one way an ingestion destroys a neighbour's data rather than merely mislabelling
//     its own (ADR-006 §2), and a run that wrote nothing at all — over which the two assertions
//     above hold the way "no foreign result" holds over an empty index;
//   - and an empty tenant_id is refused outright, before any call is built. Same line
//     internal/retrieval draws in Query.Validate, drawn here in Deps.Validate
//     (internal/ingest/note.go).
func WritePathTenantCase() Case {
	return Case{Name: "write path: every point is written under the tenant that asked for it", Run: func(ctx context.Context) string {
		runner, store := newWriteProbe()
		notes := writeNotes()

		for _, tenant := range tenants() {
			mark := len(store.calls)
			if err := runner.Ingest(ctx, tenant, notes); err != nil {
				return fmt.Sprintf("ingesting %d note(s) as %q: %v", len(notes), tenant, err)
			}
			if problem := callsCarryTenant(store.calls[mark:], tenant); problem != "" {
				return fmt.Sprintf("ingesting as %q: %s", tenant, problem)
			}
		}

		// Checked once, after every tenant has run over the same uids, because that is the only
		// moment at which a prune scoped to the uid alone has had the chance to take an earlier
		// tenant's points with it.
		//
		// This is also where "empty for the wrong reason" is caught, and it is the only place it is
		// checked: everything callsCarryTenant asserts about payloads holds vacuously over a run that
		// wrote nothing, and a run that wrote nothing lands here as a tenant holding no point. One
		// check for both, rather than a second count of what was handed over that would go stale
		// beside it.
		for _, tenant := range tenants() {
			for _, n := range notes {
				held := store.held(tenant, n.UID)
				if len(held) == 0 {
					return fmt.Sprintf("%q holds no point for the uid %s it ingested: either its own "+
						"run never wrote the note, or a later tenant's run took the points with it. "+
						"Either way every assertion above about what the payloads carry held over "+
						"nothing", tenant, n.UID)
				}
				for _, rec := range held {
					if got := payloadTenant(rec.Fields); got != tenant {
						return fmt.Sprintf("the point stored for %q at uid %s chunk %d carries "+
							"tenant_id=%q; the search filters on that field, so it answers to %q",
							tenant, n.UID, rec.ChunkIndex, got, got)
					}
				}
			}
		}

		// The write-path twin of AdversarialTenantEmptyCase. An empty tenant_id is the one value
		// internal/ingest refuses outright (Deps.Validate, note.go), and it has to be refused before
		// anything is asked of the store — a run with no scope that reads a whole tenant on the way to
		// being rejected has already asked a question it had no scope for.
		mark := len(store.calls)
		if err := runner.Ingest(ctx, "", notes); err == nil {
			return "an ingestion with no tenant_id at all was accepted; every point it wrote would " +
				"carry an empty tenant_id and answer to no tenant-scoped search"
		}
		if reached := store.calls[mark:]; len(reached) > 0 {
			return fmt.Sprintf("an ingestion with no tenant_id was refused and still reached the store "+
				"as %s scoped to %q; a run with no scope must be refused before any call is built",
				reached[0].Method, reached[0].TenantID)
		}
		return ""
	}}
}

// callsCarryTenant checks every call one run made, and the payload of every point it handed over.
//
// Both halves are here rather than in two helpers because they are the two facts that have to agree,
// and a caller that could invoke one without the other is a caller that can check half of a scope.
//
// Both hold vacuously over a run that handed over nothing, and neither says so: that half of the
// case lives at its one call site, where the stored state is what answers it. See the comment on the
// survival loop in WritePathTenantCase.
func callsCarryTenant(calls []writeCall, tenantID string) string {
	for i, c := range calls {
		if c.TenantID != tenantID {
			return fmt.Sprintf("call %d (%s) went out scoped to %q: every read, write and prune this "+
				"path makes is keyed on tenant_id + uid, and one that names another tenant reads or "+
				"deletes that tenant's points (ADR-006 §2)", i+1, c.Method, c.TenantID)
		}
		for _, p := range c.Points {
			if got := payloadTenant(p.Fields); got != tenantID {
				return fmt.Sprintf("call %d (%s) handed over a point for uid %s chunk %d whose payload "+
					"carries tenant_id=%q; the point ID is built from the call's scope and the payload "+
					"from these fields (internal/store/points.go), so the point would be identified "+
					"under %q and returned to %q", i+1, c.Method, c.UID, p.ChunkIndex, got, tenantID, got)
			}
		}
	}
	return ""
}

// payloadTenant reads tenant_id out of a payload. A missing or non-string value answers with a
// value no tenant can equal rather than with the empty string, which is itself a tenant_id the
// pipeline has a specific opinion about.
func payloadTenant(fields map[string]any) string {
	v, ok := fields[payloadTenantField]
	if !ok {
		return "\x00absent"
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("\x00non-string:%T", v)
	}
	return s
}
