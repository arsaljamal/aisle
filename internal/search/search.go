// Package search combines keyword and vector retrieval. Hybrid mode merges
// both lists with Reciprocal Rank Fusion: no score tuning needed, and a
// product that ranks well in either list rises to the top.
package search

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/errgroup"

	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/store"
)

var (
	tracer = otel.Tracer("github.com/arsaljamal/aisle/internal/search")
	meter  = otel.Meter("github.com/arsaljamal/aisle/internal/search")
)

type Mode string

const (
	Keyword Mode = "keyword"
	Vector  Mode = "vector"
	Hybrid  Mode = "hybrid"
)

type Embedder interface {
	Query(ctx context.Context, text string) ([]float32, error)
}

type Retriever interface {
	Keyword(ctx context.Context, query string, limit int) ([]store.Hit, error)
	Vector(ctx context.Context, vec []float32, limit int) ([]store.Hit, error)
}

type Searcher struct {
	Store  Retriever
	Embed  Embedder
	Faults *faults.Set
	// Depth is how many candidates each side contributes to fusion.
	Depth int
	// Version is reported on every span and metric, so a bad rollout shows up.
	Version func() string

	duration metric.Float64Histogram
	zero     metric.Int64Counter
	errs     metric.Int64Counter
}

func New(st Retriever, em Embedder, f *faults.Set, version func() string) (*Searcher, error) {
	s := &Searcher{Store: st, Embed: em, Faults: f, Depth: 50, Version: version}
	var err error
	if s.duration, err = meter.Float64Histogram("aisle.search.duration",
		metric.WithUnit("s"), metric.WithDescription("End-to-end search time"),
		metric.WithExplicitBucketBoundaries(.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10)); err != nil {
		return nil, err
	}
	if s.zero, err = meter.Int64Counter("aisle.search.zero_results",
		metric.WithDescription("Searches that returned nothing")); err != nil {
		return nil, err
	}
	if s.errs, err = meter.Int64Counter("aisle.search.errors",
		metric.WithDescription("Searches that failed")); err != nil {
		return nil, err
	}
	return s, nil
}

var ErrFusion = errors.New("fusion: rank lists out of sync")

func (s *Searcher) Search(ctx context.Context, query string, mode Mode, limit int) (hits []store.Hit, err error) {
	if mode == "" {
		mode = Hybrid
	}
	version := s.Version()
	attrs := []attribute.KeyValue{attribute.String("search.mode", string(mode)), attribute.String("app.version", version)}
	ctx, span := tracer.Start(ctx, "search."+string(mode))
	span.SetAttributes(append(attrs, attribute.String("search.query", query), attribute.Int("search.limit", limit))...)
	start := time.Now()
	defer func() {
		set := metric.WithAttributes(attrs...)
		s.duration.Record(ctx, time.Since(start).Seconds(), set)
		if err != nil {
			s.errs.Add(ctx, 1, set)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			slog.ErrorContext(ctx, "search failed", "query", query, "mode", mode, "version", version, "error", err)
		} else if len(hits) == 0 {
			s.zero.Add(ctx, 1, set)
		}
		span.SetAttributes(attribute.Int("search.results", len(hits)))
		span.End()
	}()

	switch mode {
	case Keyword:
		return s.Store.Keyword(ctx, query, limit)
	case Vector:
		vec, err := s.Embed.Query(ctx, query)
		if err != nil {
			return nil, err
		}
		return s.Store.Vector(ctx, vec, limit)
	}

	var kw, vec []store.Hit
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		kw, err = s.Store.Keyword(gctx, query, s.Depth)
		return err
	})
	g.Go(func() error {
		v, err := s.Embed.Query(gctx, query)
		if err != nil {
			return err
		}
		vec, err = s.Store.Vector(gctx, v, s.Depth)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if s.Faults.On(faults.BadDeploy) && rand.Float64() < 0.2 {
		return nil, ErrFusion
	}
	return Fuse(limit, kw, vec), nil
}

// Fuse merges ranked lists with Reciprocal Rank Fusion (k = 60).
func Fuse(limit int, lists ...[]store.Hit) []store.Hit {
	const k = 60.0
	score := map[int64]float64{}
	first := map[int64]store.Hit{}
	for _, list := range lists {
		for rank, h := range list {
			score[h.ID] += 1 / (k + float64(rank+1))
			if _, ok := first[h.ID]; !ok {
				first[h.ID] = h
			}
		}
	}
	out := make([]store.Hit, 0, len(score))
	for id, sc := range score {
		h := first[id]
		h.Score = float32(sc)
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
