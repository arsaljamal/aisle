// Package server exposes search over gRPC and a small HTTP side door for
// health checks, quick browser tests and the break switches.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	aislev1 "github.com/arsaljamal/aisle/gen/aisle/v1"
	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/eval"
	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/rag"
	"github.com/arsaljamal/aisle/internal/search"
	"github.com/arsaljamal/aisle/internal/store"
)

var meter = otel.Meter("github.com/arsaljamal/aisle/internal/server")

// Version flips to BadVersion while the bad-deploy switch is on.
const (
	GoodVersion = "1.4.0"
	BadVersion  = "1.4.1"
)

type Server struct {
	aislev1.UnimplementedSearchServiceServer
	Searcher *search.Searcher
	Answerer *rag.Answerer
	Faults   *faults.Set
	// Known maps labelled WANDS query text to its relevance labels. Searches
	// that match one are scored live, standing in for click-through data.
	Known      map[string]map[int64]int
	EmbedModel string
	// Investigator is the command paged from the control room, e.g.
	// [drill run --config ops/drill.yaml]; the alert text is appended.
	Investigator    []string
	InvestigatorDir string
	Live            Live
	Traffic         Traffic

	// HistoryFile keeps past investigations (JSON lines) for the scoreboard.
	HistoryFile string

	investigating sync.Mutex
	historyMu     sync.Mutex
	history       []Investigation
	version       atomic.Value
	relevance     metric.Float64Histogram
}

func New(s *search.Searcher, a *rag.Answerer, f *faults.Set, known map[string]map[int64]int, embedModel string) (*Server, error) {
	srv := &Server{Searcher: s, Answerer: a, Faults: f, Known: known, EmbedModel: embedModel}
	srv.version.Store(GoodVersion)
	var err error
	srv.relevance, err = meter.Float64Histogram("aisle.search.relevance",
		metric.WithDescription("NDCG@10 of live searches that match a labelled query (1 = perfect)"),
		metric.WithExplicitBucketBoundaries(.1, .2, .3, .4, .5, .6, .7, .8, .9, 1))
	return srv, err
}

func (s *Server) Version() string { return s.version.Load().(string) }

func KnownQueries(qs []catalog.Query, labels catalog.Labels) map[string]map[int64]int {
	out := map[string]map[int64]int{}
	for _, q := range qs {
		if l, ok := labels[q.ID]; ok {
			out[normalise(q.Text)] = l
		}
	}
	return out
}

func normalise(q string) string { return strings.Join(strings.Fields(strings.ToLower(q)), " ") }

var modeFromProto = map[aislev1.Mode]search.Mode{
	aislev1.Mode_MODE_UNSPECIFIED: search.Hybrid,
	aislev1.Mode_MODE_HYBRID:      search.Hybrid,
	aislev1.Mode_MODE_KEYWORD:     search.Keyword,
	aislev1.Mode_MODE_VECTOR:      search.Vector,
}

var modeToProto = map[search.Mode]aislev1.Mode{
	search.Hybrid: aislev1.Mode_MODE_HYBRID, search.Keyword: aislev1.Mode_MODE_KEYWORD, search.Vector: aislev1.Mode_MODE_VECTOR,
}

func clampLimit(n int32, def int) int {
	switch {
	case n <= 0:
		return def
	case n > 50:
		return 50
	}
	return int(n)
}

func (s *Server) Search(ctx context.Context, req *aislev1.SearchRequest) (*aislev1.SearchResponse, error) {
	q := strings.TrimSpace(req.GetQuery())
	if q == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	mode, ok := modeFromProto[req.GetMode()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "unknown mode")
	}
	start := time.Now()
	hits, err := s.search(ctx, q, mode, clampLimit(req.GetLimit(), 10))
	if err != nil {
		return nil, toStatus(err)
	}
	return &aislev1.SearchResponse{Products: toProto(hits), Mode: modeToProto[mode], TookMs: time.Since(start).Milliseconds()}, nil
}

func (s *Server) search(ctx context.Context, q string, mode search.Mode, limit int) ([]store.Hit, error) {
	start := time.Now()
	hits, err := s.Searcher.Search(ctx, q, mode, limit)
	s.Live.record(time.Since(start), err)
	if err == nil {
		if labels, ok := s.Known[normalise(q)]; ok {
			ranked := make([]int64, len(hits))
			for i, h := range hits {
				ranked[i] = h.ID
			}
			ndcg := eval.NDCG(ranked, labels, 10)
			if mode == search.Hybrid {
				s.Live.relevance(ndcg)
			}
			s.relevance.Record(ctx, ndcg, metric.WithAttributes(
				attribute.String("search.mode", string(mode)), attribute.String("app.version", s.Version())))
		}
	}
	return hits, err
}

func (s *Server) Ask(ctx context.Context, req *aislev1.AskRequest) (*aislev1.AskResponse, error) {
	q := strings.TrimSpace(req.GetQuestion())
	if q == "" {
		return nil, status.Error(codes.InvalidArgument, "question is required")
	}
	start := time.Now()
	ans, err := s.Answerer.Ask(ctx, q, clampLimit(req.GetMaxProducts(), 8))
	if err != nil {
		slog.ErrorContext(ctx, "ask failed", "error", err, "model", s.Answerer.Chat.Model)
		return nil, toStatus(err)
	}
	return &aislev1.AskResponse{Answer: ans.Text, Cited: toProto(ans.Cited), Considered: toProto(ans.Considered),
		TookMs: time.Since(start).Milliseconds()}, nil
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func toProto(hits []store.Hit) []*aislev1.Product {
	out := make([]*aislev1.Product, len(hits))
	for i, h := range hits {
		out[i] = &aislev1.Product{Id: h.ID, Name: h.Name, ProductClass: h.Class, Category: h.Category,
			Score: h.Score, AverageRating: h.Rating}
	}
	return out
}

// HTTP is the side door: /healthz, /search?q=, /ask?q=, /admin/faults.
func (s *Server) HTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.Version()})
	})
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		mode := search.Mode(r.URL.Query().Get("mode"))
		if mode == "" {
			mode = search.Hybrid
		}
		hits, err := s.search(r.Context(), r.URL.Query().Get("q"), mode, clampLimit(int32(limit), 10))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, hits)
	})
	mux.HandleFunc("GET /ask", func(w http.ResponseWriter, r *http.Request) {
		ans, err := s.Answerer.Ask(r.Context(), r.URL.Query().Get("q"), 8)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, ans)
	})
	mux.Handle("GET /", http.FileServerFS(uiFS()))
	mux.HandleFunc("GET /judge", func(w http.ResponseWriter, r *http.Request) {
		labels, ok := s.Known[normalise(r.URL.Query().Get("q"))]
		gains := map[string]int{}
		for id, g := range labels {
			gains[strconv.FormatInt(id, 10)] = g
		}
		writeJSON(w, http.StatusOK, map[string]any{"known": ok, "gains": gains})
	})
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"series": s.Live.series(), "traffic_rps": s.Traffic.current(),
			"active": s.Faults.Active(), "version": s.Version(), "investigator": len(s.Investigator) > 0})
	})
	mux.HandleFunc("POST /admin/traffic", func(w http.ResponseWriter, r *http.Request) {
		rps, _ := strconv.ParseFloat(r.URL.Query().Get("rps"), 64)
		s.Traffic.set(s, min(rps, 20))
		writeJSON(w, http.StatusOK, map[string]any{"traffic_rps": s.Traffic.current()})
	})
	mux.HandleFunc("GET /investigate", s.investigate)
	mux.HandleFunc("GET /investigations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.investigations())
	})
	mux.HandleFunc("GET /admin/faults", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"active": s.Faults.Active(), "available": faults.Catalogue})
	})
	mux.HandleFunc("POST /admin/faults/{name}", func(w http.ResponseWriter, r *http.Request) {
		name, on := r.PathValue("name"), r.URL.Query().Get("on") != "false"
		if err := s.Faults.Set(name, on); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.announce(r.Context(), name, on)
		writeJSON(w, http.StatusOK, map[string]any{"active": s.Faults.Active()})
	})
	return mux
}

// announce logs what an operator would see in real life: a deploy, a config
// reload. It never says "fault", so the investigator has to connect the dots.
func (s *Server) announce(ctx context.Context, name string, on bool) {
	switch name {
	case faults.BadDeploy:
		v := GoodVersion
		if on {
			v = BadVersion
		}
		s.version.Store(v)
		slog.InfoContext(ctx, "rollout complete: version "+v, "version", v, "change", "fusion: reuse rank buffers across requests")
	case faults.ModelMismatch:
		model := s.EmbedModel
		if on {
			model = s.EmbedModel + "-v2-preview"
		}
		slog.InfoContext(ctx, "config reloaded: embed.query_model="+model, "embed.query_model", model)
	case faults.LLMDown:
		slog.InfoContext(ctx, fmt.Sprintf("config reloaded: llm.endpoint_changed=%t", on), "llm.endpoint_changed", on)
	case faults.DBPool:
		max := 20
		if on {
			max = 1
		}
		slog.InfoContext(ctx, fmt.Sprintf("config reloaded: db.max_conns=%d", max), "db.max_conns", max)
	case faults.SlowEmbed:
		state := "ended"
		if on {
			state = "started"
		}
		slog.InfoContext(ctx, "embedding host maintenance window "+state, "started", on)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
