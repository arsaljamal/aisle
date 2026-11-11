package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	aislev1 "github.com/arsaljamal/aisle/gen/aisle/v1"
	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/faults"
)

// runLoad replays real WANDS shopper queries, mostly hybrid, with the odd Ask.
func runLoad(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	data := fs.String("data", env("AISLE_DATA", "data"), "folder with the WANDS csv files")
	addr := fs.String("addr", env("AISLE_ADDR", "localhost:7070"), "gRPC address of the server")
	rps := fs.Float64("rps", 5, "searches per second")
	dur := fs.Duration("duration", 0, "how long to run (0 = until Ctrl-C)")
	askEvery := fs.Int("ask-every", 50, "send an Ask instead of a Search every N requests (0 = never)")
	fs.Parse(args)

	qs, err := catalog.Queries(*data)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := aislev1.NewSearchServiceClient(conn)
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}

	var (
		mu      sync.Mutex
		lat     []float64
		ok, bad atomic.Int64
		n       int
		wg      sync.WaitGroup
	)
	tick := time.NewTicker(time.Duration(float64(time.Second) / *rps))
	defer tick.Stop()
	report := time.NewTicker(10 * time.Second)
	defer report.Stop()
	fmt.Printf("sending %.1f req/s to %s (Ctrl-C to stop)\n", *rps, *addr)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case <-report.C:
			mu.Lock()
			sort.Float64s(lat)
			p95 := 0.0
			if len(lat) > 0 {
				p95 = lat[int(float64(len(lat)-1)*.95)]
			}
			fmt.Printf("%s ok=%d errors=%d p95=%.0fms\n", time.Now().Format("15:04:05"), ok.Swap(0), bad.Swap(0), p95)
			lat = lat[:0]
			mu.Unlock()
		case <-tick.C:
			n++
			q := qs[rand.IntN(len(qs))].Text
			ask := *askEvery > 0 && n%*askEvery == 0
			wg.Add(1)
			go func() {
				defer wg.Done()
				rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				t := time.Now()
				var err error
				if ask {
					_, err = client.Ask(rctx, &aislev1.AskRequest{Question: "I am looking for " + q + ". What do you recommend?"})
				} else {
					mode := aislev1.Mode_MODE_HYBRID
					switch r := rand.Float64(); {
					case r < .1:
						mode = aislev1.Mode_MODE_KEYWORD
					case r < .2:
						mode = aislev1.Mode_MODE_VECTOR
					}
					_, err = client.Search(rctx, &aislev1.SearchRequest{Query: q, Mode: mode})
				}
				if err != nil {
					if ctx.Err() == nil {
						bad.Add(1)
					}
					return
				}
				ok.Add(1)
				mu.Lock()
				lat = append(lat, float64(time.Since(t).Milliseconds()))
				mu.Unlock()
			}()
		}
	}
}

func runBreak(ctx context.Context, args []string) error {
	admin := env("AISLE_ADMIN", "http://localhost:7071")
	if len(args) == 0 {
		names := make([]string, 0, len(faults.Catalogue))
		for k := range faults.Catalogue {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("  %-15s %s\n", k, faults.Catalogue[k])
		}
		return nil
	}
	on := "true"
	if len(args) > 1 && strings.EqualFold(args[1], "off") {
		on = "false"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, admin+"/admin/faults/"+args[0]+"?on="+on, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(raw, &out) != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server said %d: %s", resp.StatusCode, raw)
	}
	fmt.Fprintf(os.Stdout, "active switches: %v\n", out["active"])
	return nil
}
