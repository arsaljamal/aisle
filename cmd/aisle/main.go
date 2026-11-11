// Command aisle is a hybrid product search service with RAG answers.
//
//	aisle ingest   load the WANDS catalogue into Postgres
//	aisle embed    add vectors to every product (resumable)
//	aisle eval     score keyword, vector and hybrid search against human labels
//	aisle serve    run the gRPC API (+ HTTP side door) with full telemetry
//	aisle load     replay real shopper queries against a running server
//	aisle break    flip a break switch on a running server
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `aisle: hybrid product search with RAG answers

Usage:
  aisle ingest            load the WANDS catalogue into Postgres
  aisle embed             add vectors to every product (resumable)
  aisle eval              score keyword, vector and hybrid search
  aisle serve             run the gRPC API and HTTP side door
  aisle load              replay shopper queries against a server
  aisle break NAME [off]  flip a break switch (no NAME lists them)
  aisle version

Settings come from flags or AISLE_* environment variables; see README.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "ingest":
		err = runIngest(ctx, args)
	case "embed":
		err = runEmbed(ctx, args)
	case "eval":
		err = runEval(ctx, args)
	case "serve":
		err = runServe(ctx, args)
	case "load":
		err = runLoad(ctx, args)
	case "break":
		err = runBreak(ctx, args)
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "aisle:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// common settings shared by several subcommands.
type common struct {
	dsn, data                    string
	embedURL, embedModel, apiKey string
	dims, maxDesc                int
}

func commonFlags(fs *flag.FlagSet) *common {
	c := &common{}
	fs.StringVar(&c.dsn, "dsn", env("AISLE_DSN", "postgres://aisle:aisle@localhost:5433/aisle?sslmode=disable"), "Postgres connection string")
	fs.StringVar(&c.data, "data", env("AISLE_DATA", "data"), "folder with the WANDS csv files")
	fs.StringVar(&c.embedURL, "embed-url", env("AISLE_EMBED_URL", "http://localhost:11434/v1"), "OpenAI-compatible embeddings endpoint")
	fs.StringVar(&c.embedModel, "embed-model", env("AISLE_EMBED_MODEL", "nomic-embed-text"), "embedding model")
	fs.StringVar(&c.apiKey, "api-key", env("AISLE_API_KEY", ""), "API key for hosted models (not needed for Ollama)")
	fs.IntVar(&c.dims, "dims", 768, "embedding size (768 for nomic-embed-text)")
	fs.IntVar(&c.maxDesc, "max-desc", 600, "characters of description used for indexing")
	return c
}
