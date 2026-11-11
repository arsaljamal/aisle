package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	aislev1 "github.com/arsaljamal/aisle/gen/aisle/v1"
	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/rag"
	"github.com/arsaljamal/aisle/internal/search"
	"github.com/arsaljamal/aisle/internal/server"
	"github.com/arsaljamal/aisle/internal/store"
	"github.com/arsaljamal/aisle/internal/telemetry"
)

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c := commonFlags(fs)
	grpcAddr := fs.String("grpc", env("AISLE_GRPC_ADDR", ":7070"), "gRPC listen address")
	httpAddr := fs.String("http", env("AISLE_HTTP_ADDR", ":7071"), "HTTP side-door listen address")
	chatURL := fs.String("chat-url", env("AISLE_CHAT_URL", "http://localhost:11434/v1"), "OpenAI-compatible chat endpoint")
	chatModel := fs.String("chat-model", env("AISLE_CHAT_MODEL", "llama3.2:3b"), "answer model")
	ef := fs.Int("ef-search", 100, "HNSW search breadth")
	fs.Parse(args)

	shutdown, err := telemetry.Start(ctx, "aisle", server.GoodVersion)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()

	f := faults.New()
	st, err := store.Open(ctx, c.dsn, *ef)
	if err != nil {
		return err
	}
	defer st.Close()
	st.Faults = f
	em := embedder(c)
	em.Faults = f

	var srv *server.Server
	s, err := search.New(st, em, f, func() string { return srv.Version() })
	if err != nil {
		return err
	}
	a, err := rag.New(s, rag.Chat{BaseURL: *chatURL, Model: *chatModel, APIKey: c.apiKey}, f)
	if err != nil {
		return err
	}
	known := map[string]map[int64]int{}
	if qs, err := catalog.Queries(c.data); err == nil {
		if labels, err := catalog.LoadLabels(c.data); err == nil {
			known = server.KnownQueries(qs, labels)
		}
	}
	if srv, err = server.New(s, a, f, known, c.embedModel); err != nil {
		return err
	}

	g := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	aislev1.RegisterSearchServiceServer(g, srv)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(g, hs)
	reflection.Register(g)
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		return err
	}
	h := &http.Server{Addr: *httpAddr, Handler: otelhttp.NewHandler(srv.HTTP(), "http"), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- g.Serve(lis) }()
	go func() { errc <- h.ListenAndServe() }()
	slog.InfoContext(ctx, "aisle serving", "grpc", *grpcAddr, "http", *httpAddr, "version", server.GoodVersion,
		"embed_model", c.embedModel, "chat_model", *chatModel, "labelled_queries", len(known))

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	hs.Shutdown()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Shutdown(sctx)
	g.GracefulStop()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
