package embed

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// validDense returns a 1024-element dense vector that satisfies every invariant, so a test can
// vary exactly one thing and know the failure came from that thing.
func validDense() []float32 {
	d := make([]float32, DefaultDenseDim())
	for i := range d {
		d[i] = 0.03125
	}
	return d
}

func validEmbedding() Embedding {
	return Embedding{
		Dense:  validDense(),
		Sparse: Sparse{Indices: []uint32{3, 17, 900}, Values: []float32{0.28, 0.22, 0.14}},
	}
}

func TestValidateEmbedding_ValidPasses(t *testing.T) {
	if err := validateDefault(validEmbedding()); err != nil {
		t.Fatalf("a valid embedding was rejected: %v", err)
	}
}

func TestValidateEmbedding(t *testing.T) {
	shortDense := validDense()[:DefaultDenseDim()-1]

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
		{
			name:    "dense wrong dimension",
			in:      Embedding{Dense: shortDense, Sparse: validEmbedding().Sparse},
			wantSub: "1023",
		},
		{
			name:    "dense contains NaN",
			in:      Embedding{Dense: nanDense, Sparse: validEmbedding().Sparse},
			wantSub: "Dense[7]",
		},
		{
			name:    "dense contains +Inf",
			in:      Embedding{Dense: infDense, Sparse: validEmbedding().Sparse},
			wantSub: "Dense[7]",
		},
		{
			name:    "dense contains -Inf",
			in:      Embedding{Dense: negInfDense, Sparse: validEmbedding().Sparse},
			wantSub: "Dense[7]",
		},
		{
			name: "sparse length mismatch",
			in: Embedding{
				Dense:  validDense(),
				Sparse: Sparse{Indices: []uint32{1, 2, 3}, Values: []float32{0.1, 0.2}},
			},
			wantSub: "length",
		},
		{
			name: "sparse indices not ascending — the server promised sorted output",
			in: Embedding{
				Dense:  validDense(),
				Sparse: Sparse{Indices: []uint32{5, 2}, Values: []float32{0.1, 0.2}},
			},
			wantSub: "ascending",
		},
		{
			name: "sparse repeated index",
			in: Embedding{
				Dense:  validDense(),
				Sparse: Sparse{Indices: []uint32{1, 1, 2}, Values: []float32{0.1, 0.2, 0.3}},
			},
			wantSub: "repeated",
		},
		{
			name: "sparse zero weight",
			in: Embedding{
				Dense:  validDense(),
				Sparse: Sparse{Indices: []uint32{1, 2}, Values: []float32{0.1, 0}},
			},
			wantSub: "zero",
		},
		{
			name: "sparse value NaN",
			in: Embedding{
				Dense:  validDense(),
				Sparse: Sparse{Indices: []uint32{1, 2}, Values: []float32{0.1, float32(math.NaN())}},
			},
			wantSub: "Sparse.Values[1]",
		},
		{
			name: "sparse value Inf",
			in: Embedding{
				Dense:  validDense(),
				Sparse: Sparse{Indices: []uint32{1, 2}, Values: []float32{0.1, float32(math.Inf(-1))}},
			},
			wantSub: "Sparse.Values[1]",
		},
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

// TestValidateEmbedding_DegenerateSparse_MatchesSentinel is what lets S06a skip one note instead of
// stopping the run: the two degenerate-sparse classes are matchable, and nothing else is.
func TestValidateEmbedding_DegenerateSparse_MatchesSentinel(t *testing.T) {
	zeroWeight := Embedding{
		Dense:  validDense(),
		Sparse: Sparse{Indices: []uint32{1, 2}, Values: []float32{0.1, 0}},
	}
	repeated := Embedding{
		Dense:  validDense(),
		Sparse: Sparse{Indices: []uint32{1, 1}, Values: []float32{0.1, 0.2}},
	}

	if err := validateDefault(zeroWeight); !errors.Is(err, ErrSparseZeroWeight) {
		t.Errorf("zero weight: errors.Is(%v, ErrSparseZeroWeight) = false", err)
	}
	if err := validateDefault(repeated); !errors.Is(err, ErrSparseRepeatedIndex) {
		t.Errorf("repeated index: errors.Is(%v, ErrSparseRepeatedIndex) = false", err)
	}
	if err := validateDefault(zeroWeight); errors.Is(err, ErrSparseRepeatedIndex) {
		t.Error("zero weight also matched ErrSparseRepeatedIndex — the two classes are not separable")
	}
	if err := validateDefault(repeated); errors.Is(err, ErrSparseZeroWeight) {
		t.Error("repeated index also matched ErrSparseZeroWeight — the two classes are not separable")
	}

	// The classes S06a must NOT skip a note for: a broken backend, not a degenerate vector.
	nan := validDense()
	nan[0] = float32(math.NaN())
	others := []Embedding{
		{Dense: validDense()[:3], Sparse: validEmbedding().Sparse},
		{Dense: nan, Sparse: validEmbedding().Sparse},
		{Dense: validDense(), Sparse: Sparse{Indices: []uint32{1, 2}, Values: []float32{0.1}}},
		{Dense: validDense(), Sparse: Sparse{Indices: []uint32{5, 2}, Values: []float32{0.1, 0.2}}},
	}
	for i, e := range others {
		err := validateDefault(e)
		if err == nil {
			t.Fatalf("case %d: expected a validation error", i)
		}
		if errors.Is(err, ErrSparseZeroWeight) || errors.Is(err, ErrSparseRepeatedIndex) {
			t.Errorf("case %d: a non-degenerate violation (%v) matched a degenerate-sparse sentinel",
				i, err)
		}
	}
}

func TestValidateEmbedding_DoesNotMutateInput(t *testing.T) {
	e := validEmbedding()
	before := Embedding{
		Dense:  slices.Clone(e.Dense),
		Sparse: Sparse{Indices: slices.Clone(e.Sparse.Indices), Values: slices.Clone(e.Sparse.Values)},
	}
	_ = validateDefault(e)
	if !reflect.DeepEqual(e, before) {
		t.Fatal("validateEmbedding mutated its input on the success path")
	}

	bad := Embedding{Dense: validDense(), Sparse: Sparse{Indices: []uint32{2, 1}, Values: []float32{0.1, 0.2}}}
	badBefore := Embedding{
		Dense:  slices.Clone(bad.Dense),
		Sparse: Sparse{Indices: slices.Clone(bad.Sparse.Indices), Values: slices.Clone(bad.Sparse.Values)},
	}
	_ = validateDefault(bad)
	if !reflect.DeepEqual(bad, badBefore) {
		t.Fatal("validateEmbedding mutated its input on the error path")
	}
}

// TestValidateEmbedding_EmptySparseIsRejected pins a case the invariant list leaves implicit: an
// all-zero sparse vector is what a backend that silently dropped its sparse head returns, and it is
// exactly the "vetor zerado silencioso" the story refuses to let through.
func TestValidateEmbedding_EmptySparseIsRejected(t *testing.T) {
	err := validateDefault(Embedding{Dense: validDense()})
	if err == nil {
		t.Fatal("an embedding with no sparse entries was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not name the empty sparse vector", err)
	}
}

// validateDefault validates against the default model profile's width, which is what these fixtures
// build (PR 1 of the embedding migration: the width is a parameter of validateEmbedding now).
func validateDefault(e Embedding) error { return validateEmbedding(e, DefaultDenseDim()) }

// The accepted width is the model profile's, not a package constant: a 768-wide vector is valid for a
// 768-wide profile and rejected for the default one.
func TestValidateEmbedding_WidthComesFromTheProfile(t *testing.T) {
	e := validEmbedding()
	e.Dense = e.Dense[:768]
	if err := validateEmbedding(e, 768); err != nil {
		t.Errorf("a 768-wide vector was rejected for a 768-wide profile: %v", err)
	}
	if err := validateEmbedding(e, DefaultDenseDim()); err == nil {
		t.Error("a 768-wide vector was accepted for the default 1024-wide profile")
	}
}
