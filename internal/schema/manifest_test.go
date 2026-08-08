package schema

import (
	"maps"
	"slices"
	"testing"

	"github.com/qdrant/go-client/qdrant"
)

// The three collection names and the nine indexed fields are spelled out here as literals on
// purpose: this test is the second, independent statement of the contract, and reading them off
// Manifest() would make it agree with itself no matter what the manifest said.

func TestManifest_ThreeCollectionsDefined(t *testing.T) {
	m := Manifest()
	if len(m) != 3 {
		t.Fatalf("len(Manifest()) = %d, want 3 — exactly three collections, no operational fourth", len(m))
	}

	names := make([]string, len(m))
	for i, c := range m {
		names[i] = c.Name
	}
	if want := []string{"interno", "clientes", "base_paga"}; !slices.Equal(names, want) {
		t.Errorf("collection names = %v, want %v", names, want)
	}

	for _, c := range m {
		if c.DenseVectorName != "dense" {
			t.Errorf("%s: DenseVectorName = %q, want %q", c.Name, c.DenseVectorName, "dense")
		}
		if c.DenseDim != 1024 {
			t.Errorf("%s: DenseDim = %d, want 1024 (PRD-contrato §2.3b)", c.Name, c.DenseDim)
		}
		if c.Distance != qdrant.Distance_Cosine {
			t.Errorf("%s: Distance = %v, want %v", c.Name, c.Distance, qdrant.Distance_Cosine)
		}
		if c.SparseVectorName != "sparse" {
			t.Errorf("%s: SparseVectorName = %q, want %q", c.Name, c.SparseVectorName, "sparse")
		}
		// The revision is referenced, not duplicated: a bump in identity.go must move the manifest
		// with it, or the model-revision drift check (Apply) would compare against a stale literal.
		if c.ModelRevision != BGEM3Revision {
			t.Errorf("%s: ModelRevision = %q, want BGEM3Revision (%q)", c.Name, c.ModelRevision, BGEM3Revision)
		}
	}
}

// TestManifest_PayloadIndexes_TenantIsTenantTrue pins the exact indexed set, because strict mode
// turns a missing index into a rejected filter rather than a slow one. The set is the union of what
// the two filtering stories need — S06a's tenant_id + uid + chunk_index (T1's ScrollByUID, T6's
// DeleteByFilter) and S07's six facets — and it is spelled out here so that adding a filter without
// its index fails a test instead of failing in production.
func TestManifest_PayloadIndexes_TenantIsTenantTrue(t *testing.T) {
	wantKinds := map[string]qdrant.FieldType{
		"tenant_id":   qdrant.FieldType_FieldTypeKeyword,
		"uid":         qdrant.FieldType_FieldTypeKeyword,
		"chunk_index": qdrant.FieldType_FieldTypeInteger,
		"status":      qdrant.FieldType_FieldTypeKeyword,
		"area":        qdrant.FieldType_FieldTypeKeyword,
		"vault":       qdrant.FieldType_FieldTypeKeyword,
		"type":        qdrant.FieldType_FieldTypeKeyword,
		"visibility":  qdrant.FieldType_FieldTypeKeyword,
		"tags":        qdrant.FieldType_FieldTypeKeyword,
	}

	for _, c := range Manifest() {
		fields := make([]string, len(c.Indexes))
		for i, idx := range c.Indexes {
			fields[i] = idx.Field
		}
		got := slices.Sorted(slices.Values(fields))
		if want := slices.Sorted(maps.Keys(wantKinds)); !slices.Equal(got, want) {
			t.Errorf("%s: indexed fields = %v, want exactly %v", c.Name, got, want)
		}

		for _, idx := range c.Indexes {
			// chunk_index is filtered with a range (>= N), which Qdrant refuses on a keyword index,
			// so the type is part of the contract and not an implementation detail.
			if want := wantKinds[idx.Field]; idx.Kind != want {
				t.Errorf("%s: index %q kind = %v, want %v", c.Name, idx.Field, idx.Kind, want)
			}
			// is_tenant is what makes Qdrant lay a collection out per tenant; setting it on a
			// second field would split the layout by something that is not the trust boundary.
			if wantTenant := idx.Field == "tenant_id"; idx.IsTenant != wantTenant {
				t.Errorf("%s: index %q IsTenant = %v, want %v", c.Name, idx.Field, idx.IsTenant, wantTenant)
			}
		}
	}
}

// TestManifest_ReturnsIndependentCopy pins the property the drift tests depend on: an integration
// test that mutates the manifest to provoke drift must not poison every later caller in the same
// process.
func TestManifest_ReturnsIndependentCopy(t *testing.T) {
	m := Manifest()
	m[0].DenseDim = 768
	m[0].Indexes[0].IsTenant = false
	m[0].Indexes = append(m[0].Indexes, PayloadIndex{Field: "smuggled"})

	fresh := Manifest()
	if fresh[0].DenseDim != 1024 {
		t.Errorf("mutating a returned manifest changed DenseDim of the next one: got %d", fresh[0].DenseDim)
	}
	if !fresh[0].Indexes[0].IsTenant {
		t.Error("mutating a returned manifest's Indexes changed the next one's tenant index")
	}
	if len(fresh[0].Indexes) != 9 {
		t.Errorf("mutating a returned manifest's Indexes changed the next one's length: got %d", len(fresh[0].Indexes))
	}
}
