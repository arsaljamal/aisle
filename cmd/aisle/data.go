package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/embed"
	"github.com/arsaljamal/aisle/internal/store"
)

func embedder(c *common) *embed.Client {
	e := &embed.Client{BaseURL: c.embedURL, Model: c.embedModel, APIKey: c.apiKey,
		HTTP: &http.Client{Timeout: 2 * time.Minute}}
	if strings.HasPrefix(c.embedModel, "nomic-embed-text") {
		e.QueryPrefix, e.DocPrefix = "search_query: ", "search_document: "
	}
	return e
}

func runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	c := commonFlags(fs)
	fs.Parse(args)
	if err := store.Migrate(ctx, c.dsn, c.dims); err != nil {
		return err
	}
	st, err := store.Open(ctx, c.dsn, 100)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Truncate(ctx); err != nil {
		return err
	}
	var batch []catalog.Product
	var total int64
	flush := func() error {
		n, err := st.Load(ctx, batch)
		total += n
		batch = batch[:0]
		return err
	}
	err = catalog.EachProduct(c.data, func(p catalog.Product) error {
		batch = append(batch, p)
		if len(batch) == 5000 {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return err
	}
	fmt.Printf("loaded %d products\n", total)
	return nil
}

func runEmbed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	c := commonFlags(fs)
	batchSize := fs.Int("batch", 64, "texts per embedding call")
	workers := fs.Int("workers", 2, "parallel embedding calls")
	fs.Parse(args)
	st, err := store.Open(ctx, c.dsn, 100)
	if err != nil {
		return err
	}
	defer st.Close()
	em := embedder(c)
	total, done, err := st.Counts(ctx)
	if err != nil {
		return err
	}
	start, startDone := time.Now(), done
	for {
		pending, err := st.Unembedded(ctx, *batchSize**workers*4, c.maxDesc)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			break
		}
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(*workers)
		for i := 0; i < len(pending); i += *batchSize {
			job := pending[i:min(i+*batchSize, len(pending))]
			g.Go(func() error {
				texts := make([]string, len(job))
				ids := make([]int64, len(job))
				for i, p := range job {
					texts[i], ids[i] = p.Doc, p.ID
				}
				vecs, err := em.Documents(gctx, texts)
				if err != nil {
					return err
				}
				return st.SetEmbeddings(gctx, ids, vecs)
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		done += int64(len(pending))
		rate := float64(done-startDone) / time.Since(start).Seconds()
		fmt.Printf("\rembedded %d/%d (%.0f/s)   ", done, total, rate)
	}
	fmt.Println("\nbuilding vector index ...")
	return st.IndexVectors(ctx)
}
