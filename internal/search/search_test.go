package search

import (
	"context"
	"errors"
	"testing"

	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/store"
)

func ids(hs []store.Hit) []int64 {
	out := make([]int64, len(hs))
	for i, h := range hs {
		out[i] = h.ID
	}
	return out
}

func TestFuseRewardsAgreement(t *testing.T) {
	kw := []store.Hit{{ID: 1}, {ID: 2}, {ID: 3}}
	vec := []store.Hit{{ID: 3}, {ID: 4}, {ID: 1}}
	got := ids(Fuse(10, kw, vec))
	// 1 and 3 appear in both lists, so they lead.
	if got[0] != 1 || got[1] != 3 {
		t.Fatalf("got %v", got)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 unique, got %v", got)
	}
}

func TestFuseRespectsLimit(t *testing.T) {
	if got := Fuse(2, []store.Hit{{ID: 1}, {ID: 2}, {ID: 3}}); len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
}

type fakeStore struct{}

func (fakeStore) Keyword(context.Context, string, int) ([]store.Hit, error) {
	return []store.Hit{{ID: 1}, {ID: 2}}, nil
}
func (fakeStore) Vector(context.Context, []float32, int) ([]store.Hit, error) {
	return []store.Hit{{ID: 2}, {ID: 3}}, nil
}

type fakeEmbed struct{ err error }

func (f fakeEmbed) Query(context.Context, string) ([]float32, error) { return []float32{1}, f.err }

func newSearcher(t *testing.T, em Embedder, f *faults.Set) *Searcher {
	t.Helper()
	s, err := New(fakeStore{}, em, f, func() string { return "test" })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHybridMergesBothSides(t *testing.T) {
	got, err := newSearcher(t, fakeEmbed{}, faults.New()).Search(context.Background(), "sofa", Hybrid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got)[0] != 2 || len(got) != 3 {
		t.Fatalf("got %v", ids(got))
	}
}

func TestEmbedFailureFailsHybrid(t *testing.T) {
	_, err := newSearcher(t, fakeEmbed{err: errors.New("down")}, faults.New()).Search(context.Background(), "sofa", Hybrid, 10)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestBadDeployFailsSomeSearches(t *testing.T) {
	f := faults.New()
	_ = f.Set(faults.BadDeploy, true)
	s := newSearcher(t, fakeEmbed{}, f)
	failed := 0
	for range 500 {
		if _, err := s.Search(context.Background(), "sofa", Hybrid, 10); errors.Is(err, ErrFusion) {
			failed++
		}
	}
	if failed < 50 || failed > 160 {
		t.Fatalf("expected about 20%% failures, got %d/500", failed)
	}
}
