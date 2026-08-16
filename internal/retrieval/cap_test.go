package retrieval

import (
	"testing"
)

func TestCapPerUID(t *testing.T) {
	a := func(index int) Result { return Result{UID: "a", ChunkIndex: index} }
	b := func(index int) Result { return Result{UID: "b", ChunkIndex: index} }
	c := func(index int) Result { return Result{UID: "c", ChunkIndex: index} }
	d := func(index int) Result { return Result{UID: "d", ChunkIndex: index} }

	cases := []struct {
		name      string
		in        []Result
		topK      int
		maxPerUID int
		want      []Result
	}{
		{
			name:      "five of one note, cap 2",
			in:        []Result{a(0), a(1), a(2), a(3), a(4)},
			topK:      5,
			maxPerUID: 2,
			want:      []Result{a(0), a(1)},
		},
		{
			name:      "keeps first two of each then fills topK",
			in:        []Result{a(0), a(1), a(2), b(0), b(1), c(0)},
			topK:      5,
			maxPerUID: 2,
			want:      []Result{a(0), a(1), b(0), b(1), c(0)},
		},
		{
			name:      "drops the third of a to make room for another note",
			in:        []Result{a(0), a(1), a(2), b(0), b(1), c(0), d(0)},
			topK:      5,
			maxPerUID: 2,
			want:      []Result{a(0), a(1), b(0), b(1), c(0)},
		},
		{
			name:      "cap 0 is no diversity, only the topK window",
			in:        []Result{a(0), a(1), a(2), b(0)},
			topK:      3,
			maxPerUID: 0,
			want:      []Result{a(0), a(1), a(2)},
		},
		{
			name:      "fewer hits than topK",
			in:        []Result{a(0), b(0)},
			topK:      5,
			maxPerUID: 2,
			want:      []Result{a(0), b(0)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capPerUID(tc.in, tc.topK, tc.maxPerUID)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].UID != tc.want[i].UID || got[i].ChunkIndex != tc.want[i].ChunkIndex {
					t.Errorf("result[%d] = %s#%d, want %s#%d",
						i, got[i].UID, got[i].ChunkIndex, tc.want[i].UID, tc.want[i].ChunkIndex)
				}
			}
		})
	}
}

func TestSearch_OverFetchesThenCapsPerUID(t *testing.T) {
	points := []ScoredPoint{
		notePointUID("p0", "note-a", 0, 0.9),
		notePointUID("p1", "note-a", 1, 0.8),
		notePointUID("p2", "note-a", 2, 0.7),
		notePointUID("p3", "note-b", 0, 0.6),
		notePointUID("p4", "note-c", 0, 0.5),
	}
	e, x := &spyEmbedder{}, &spyExecutor{points: points}
	s := NewSearcher(e, x, DefaultConfig())

	q := validQuery()
	q.TopK = 3
	got, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if x.requests[0].Limit != q.TopK*DefaultMaxChunksPerUID {
		t.Errorf("Limit = %d, want the over-fetch window %d",
			x.requests[0].Limit, q.TopK*DefaultMaxChunksPerUID)
	}
	wantPrefetch := calibratedPrefetchLimit(Query{TopK: q.TopK * DefaultMaxChunksPerUID}, s.cfg.PrefetchMultiplier)
	if x.requests[0].Prefetch[0].Limit != wantPrefetch {
		t.Errorf("prefetch limit = %d, want %d so RRF still has a wide enough pool",
			x.requests[0].Prefetch[0].Limit, wantPrefetch)
	}

	if len(got) != 3 {
		t.Fatalf("%d result(s), want topK 3 after the cap", len(got))
	}
	if got[0].UID != "note-a" || got[1].UID != "note-a" {
		t.Errorf("first two should be note-a, got %s then %s", got[0].UID, got[1].UID)
	}
	if got[2].UID != "note-b" {
		t.Errorf("third should be note-b (the third note-a was dropped), got %s#%d", got[2].UID, got[2].ChunkIndex)
	}
}

func TestSearch_CapDisabled_KeepsQdrantWindow(t *testing.T) {
	points := []ScoredPoint{
		notePointUID("p0", "note-a", 0, 0.9),
		notePointUID("p1", "note-a", 1, 0.8),
		notePointUID("p2", "note-a", 2, 0.7),
	}
	e, x := &spyEmbedder{}, &spyExecutor{points: points}
	s := NewSearcher(e, x, Config{PrefetchMultiplier: DefaultPrefetchMultiplier})

	q := validQuery()
	q.TopK = 3
	got, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if x.requests[0].Limit != q.TopK {
		t.Errorf("Limit = %d, want the asked topK %d when the cap is off", x.requests[0].Limit, q.TopK)
	}
	if len(got) != 3 || got[2].UID != "note-a" {
		t.Errorf("cap-off should keep all three of note-a, got %+v", got)
	}
}

func TestDefaultConfig_CapsAtTwoPerNote(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxChunksPerUID != DefaultMaxChunksPerUID || DefaultMaxChunksPerUID != 2 {
		t.Errorf("DefaultConfig.MaxChunksPerUID = %d, DefaultMaxChunksPerUID = %d, want 2",
			cfg.MaxChunksPerUID, DefaultMaxChunksPerUID)
	}
}

func notePointUID(id, uid string, index int, score float32) ScoredPoint {
	p := scoredPoint(id, score)
	p.Payload[fieldUID] = uid
	p.Payload[fieldChunkIndex] = index
	return p
}
