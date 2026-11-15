package embed

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/danielmalka/go-knowrag/internal/schema"
)

// validDense returns a dense vector of the default profile's width that satisfies every invariant, so
// a test can vary exactly one thing and know the failure came from that thing.
func validDense() []float32 {
	d := make([]float32, DefaultDenseDim())
	for i := range d {
		d[i] = 0.03125
	}
	return d
}

func validEmbedding() Embedding { return Embedding{Dense: validDense()} }

// validateDefault validates against the default model profile's width, which is what these fixtures
// build.
func validateDefault(e Embedding) error { return validateEmbedding(e, schema.DefaultProfile()) }

func TestValidateEmbedding_ValidPasses(t *testing.T) {
	if err := validateDefault(validEmbedding()); err != nil {
		t.Fatalf("a valid embedding was rejected: %v", err)
	}
}

func TestValidateEmbedding(t *testing.T) {
	nanDense := validDense()
	nanDense[7] = float32(math.NaN())

	infDense := validDense()
	infDense[7] = float32(math.Inf(1))

	negInfDense := validDense()
	negInfDense[7] = float32(math.Inf(-1))

	cases := []struct {
		name    string
		in      Embedding
		wantSub string // substring the message must name, so the operator knows which invariant broke
	}{
		{"one element short", Embedding{Dense: validDense()[:DefaultDenseDim()-1]}, "767"},
		{"one element long", Embedding{Dense: append(validDense(), 0.1)}, "769"},
		{"empty", Embedding{}, "0 elements"},
		{"dense contains NaN", Embedding{Dense: nanDense}, "Dense[7]"},
		{"dense contains +Inf", Embedding{Dense: infDense}, "Dense[7]"},
		{"dense contains -Inf", Embedding{Dense: negInfDense}, "Dense[7]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDefault(tc.in)
			if err == nil {
				t.Fatalf("no error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateEmbedding_DoesNotMutateInput(t *testing.T) {
	e := validEmbedding()
	before := Embedding{Dense: slices.Clone(e.Dense)}
	_ = validateDefault(e)
	if !reflect.DeepEqual(e, before) {
		t.Fatal("validateEmbedding mutated its input on the success path")
	}

	nan := validDense()
	nan[0] = float32(math.NaN())
	bad := Embedding{Dense: nan}
	_ = validateDefault(bad)
	if !math.IsNaN(float64(bad.Dense[0])) || len(bad.Dense) != DefaultDenseDim() {
		t.Fatal("validateEmbedding mutated its input on the error path")
	}
}

// The accepted width is the model profile's, not a package constant: a vector of another width is valid
// for a profile of that width and rejected for the default one.
func TestValidateEmbedding_WidthComesFromTheProfile(t *testing.T) {
	other := schema.DefaultProfile()
	other.ID, other.DenseDim = "other", 512
	dense := make([]float32, 512)
	if err := validateEmbedding(Embedding{Dense: dense}, other); err != nil {
		t.Errorf("a 512-wide vector was rejected for a 512-wide profile: %v", err)
	}
	if err := validateEmbedding(Embedding{Dense: dense}, schema.DefaultProfile()); err == nil {
		t.Error("a 512-wide vector was accepted for the default 768-wide profile")
	}
}
