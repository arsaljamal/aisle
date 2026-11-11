package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/eval"
	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/search"
	"github.com/arsaljamal/aisle/internal/store"
)

type modeResult struct {
	Mode        string  `json:"mode"`
	Queries     int     `json:"queries"`
	NDCG10      float64 `json:"ndcg_at_10"`
	ExactAt10   float64 `json:"exact_precision_at_10"`
	MRR         float64 `json:"mrr"`
	ZeroResults int     `json:"zero_results"`
	P50ms       float64 `json:"p50_ms"`
	P95ms       float64 `json:"p95_ms"`
}

func runEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	c := commonFlags(fs)
	out := fs.String("out", "results/eval.json", "where to write the scores")
	ef := fs.Int("ef-search", 100, "HNSW search breadth (higher = more accurate, slower)")
	fs.Parse(args)

	queries, err := catalog.Queries(c.data)
	if err != nil {
		return err
	}
	labels, err := catalog.LoadLabels(c.data)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, c.dsn, *ef)
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := search.New(st, embedder(c), faults.New(), func() string { return version })
	if err != nil {
		return err
	}

	var results []modeResult
	fmt.Printf("%-8s %8s %8s %8s %6s %8s %8s\n", "mode", "NDCG@10", "Exact@10", "MRR", "zero", "p50 ms", "p95 ms")
	for _, mode := range []search.Mode{search.Keyword, search.Vector, search.Hybrid} {
		r := modeResult{Mode: string(mode)}
		var lat []float64
		for _, q := range queries {
			gains, ok := labels[q.ID]
			if !ok {
				continue
			}
			t := time.Now()
			hits, err := s.Search(ctx, q.Text, mode, 10)
			if err != nil {
				return fmt.Errorf("%s %q: %w", mode, q.Text, err)
			}
			lat = append(lat, float64(time.Since(t).Microseconds())/1000)
			ranked := make([]int64, len(hits))
			for i, h := range hits {
				ranked[i] = h.ID
			}
			if len(hits) == 0 {
				r.ZeroResults++
			}
			r.NDCG10 += eval.NDCG(ranked, gains, 10)
			r.ExactAt10 += eval.PrecisionExact(ranked, gains, 10)
			r.MRR += eval.ReciprocalRank(ranked, gains)
			r.Queries++
		}
		n := float64(r.Queries)
		r.NDCG10, r.ExactAt10, r.MRR = r.NDCG10/n, r.ExactAt10/n, r.MRR/n
		sort.Float64s(lat)
		r.P50ms, r.P95ms = eval.Percentile(lat, .5), eval.Percentile(lat, .95)
		results = append(results, r)
		fmt.Printf("%-8s %8.3f %8.3f %8.3f %6d %8.1f %8.1f\n", r.Mode, r.NDCG10, r.ExactAt10, r.MRR, r.ZeroResults, r.P50ms, r.P95ms)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{
		"dataset": "WANDS (Wayfair, 42,994 products, 480 queries)", "embed_model": c.embedModel,
		"ef_search": *ef, "results": results,
	}, "", "  ")
	return os.WriteFile(*out, append(b, '\n'), 0o644)
}
