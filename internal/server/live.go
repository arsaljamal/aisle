package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/search"
)

// Live keeps the last few minutes of request outcomes in memory so the
// control room can draw charts without a metrics backend.
type Live struct {
	mu      sync.Mutex
	buckets [bucketCount]bucket
}

const (
	bucketWidth = 2 * time.Second
	bucketCount = 90 // three minutes
)

type bucket struct {
	at             int64 // unix seconds of the bucket start
	n, errs        int
	durs           []float64
	ndcgSum, ndcgN float64
}

func (l *Live) slot(now time.Time) *bucket {
	at := now.Truncate(bucketWidth).Unix()
	b := &l.buckets[(at/int64(bucketWidth.Seconds()))%bucketCount]
	if b.at != at {
		*b = bucket{at: at}
	}
	return b
}

func (l *Live) record(d time.Duration, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.slot(time.Now())
	b.n++
	if err != nil {
		b.errs++
	}
	b.durs = append(b.durs, d.Seconds()*1000)
}

func (l *Live) relevance(v float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.slot(time.Now())
	b.ndcgSum += v
	b.ndcgN++
}

type point struct {
	T    int64    `json:"t"`
	RPS  float64  `json:"rps"`
	Err  float64  `json:"err"`
	P95  float64  `json:"p95_ms"`
	NDCG *float64 `json:"ndcg"`
}

func (l *Live) series() []point {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Truncate(bucketWidth).Unix()
	step := int64(bucketWidth.Seconds())
	out := make([]point, 0, bucketCount-1)
	for at := now - step*(bucketCount-1); at < now; at += step { // skip the bucket still filling
		b := l.buckets[(at/step)%bucketCount]
		p := point{T: at}
		if b.at == at && b.n > 0 {
			p.RPS = float64(b.n) / bucketWidth.Seconds()
			p.Err = float64(b.errs) / float64(b.n)
			d := append([]float64(nil), b.durs...)
			sort.Float64s(d)
			p.P95 = d[int(float64(len(d)-1)*.95)]
			if b.ndcgN > 0 {
				v := b.ndcgSum / b.ndcgN
				p.NDCG = &v
			}
		}
		out = append(out, p)
	}
	return out
}

// Traffic replays real shopper queries inside the server.
type Traffic struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	rps     float64
	Queries []catalog.Query
}

func (t *Traffic) set(s *Server, rps float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.rps = rps
	if rps <= 0 || len(t.Queries) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	go func() {
		tick := time.NewTicker(time.Duration(float64(time.Second) / rps))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				q := t.Queries[rand.IntN(len(t.Queries))].Text
				mode := search.Hybrid
				switch r := rand.Float64(); {
				case r < .1:
					mode = search.Keyword
				case r < .2:
					mode = search.Vector
				}
				go func() {
					rctx, c := context.WithTimeout(ctx, 5*time.Second)
					defer c()
					_, _ = s.search(rctx, q, mode, 10)
				}()
			}
		}
	}()
}

func (t *Traffic) current() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rps
}

// Alerts the control room can page the investigator with. Fixed text only:
// the page never passes free text to the command.
var Alerts = map[string]string{
	"latency":   "PAGE: aisle search p95 latency is above 1s (SLO 300ms). Investigate and find the root cause.",
	"relevance": "PAGE: aisle live search relevance (NDCG@10) has dropped below 0.5. No errors reported. Investigate and find the root cause.",
	"errors":    "PAGE: aisle search error rate is above 5%. Investigate and find the root cause.",
	"ask":       "PAGE: aisle Ask endpoint is failing. Investigate and find the root cause.",
}

// Investigation is one paging of the investigator, kept for the scoreboard.
type Investigation struct {
	At      time.Time         `json:"at"`
	Alert   string            `json:"alert"`
	Active  []string          `json:"active"` // switches on when paged: the true cause
	Seconds float64           `json:"seconds"`
	Turns   int               `json:"turns"`
	Queries int               `json:"queries"`
	Nudges  int               `json:"nudges"` // times the report gate sent the report back
	Tokens  int64             `json:"tokens"`
	Stop    string            `json:"stop"`
	Verdict string            `json:"verdict"`
	Report  string            `json:"report"`
	Steps   []json.RawMessage `json:"steps,omitempty"` // every harness event, for replays
}

// investigate runs the configured investigator command with the alert as its
// last argument and streams every output line as a server-sent event. Run the
// investigator with --events json so steps arrive as JSON objects.
func (s *Server) investigate(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("alert")
	alert, ok := Alerts[key]
	if !ok || len(s.Investigator) == 0 {
		http.Error(w, "unknown alert or no investigator configured (serve --investigator)", http.StatusBadRequest)
		return
	}
	if !s.investigating.TryLock() {
		http.Error(w, "an investigation is already running", http.StatusConflict)
		return
	}
	defer s.investigating.Unlock()
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
	rec := Investigation{At: time.Now(), Alert: key, Active: s.Faults.Active()}
	args := append(append([]string(nil), s.Investigator[1:]...), alert)
	cmd := exec.CommandContext(r.Context(), s.Investigator[0], args...)
	cmd.Dir = s.InvestigatorDir
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		send("error", err.Error())
		return
	}
	send("alert", map[string]any{"text": alert, "active": rec.Active})
	lines := make(chan [2]string)
	var wg sync.WaitGroup
	for kind, pipe := range map[string]io.Reader{"report": stdout, "step": stderr} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := bufio.NewScanner(pipe)
			sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
			for sc.Scan() {
				lines <- [2]string{kind, sc.Text()}
			}
		}()
	}
	go func() { wg.Wait(); close(lines) }()
	var report strings.Builder
	for l := range lines {
		if l[0] == "report" {
			report.WriteString(l[1] + "\n")
			send("report", l[1])
			continue
		}
		var ev struct {
			Kind         string `json:"kind"`
			Turn         int    `json:"turn"`
			Stop         string `json:"stop"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
		}
		if json.Unmarshal([]byte(l[1]), &ev) != nil || ev.Kind == "" {
			continue // tool server logs, not investigator steps
		}
		switch ev.Kind {
		case "tool_call":
			rec.Queries++
		case "gate":
			rec.Nudges++
		case "summary":
			rec.Turns, rec.Stop, rec.Tokens = ev.Turn, ev.Stop, ev.InputTokens+ev.OutputTokens
		}
		rec.Steps = append(rec.Steps, json.RawMessage(l[1]))
		send("step", json.RawMessage(l[1]))
	}
	_ = cmd.Wait()
	rec.Seconds = time.Since(rec.At).Seconds()
	rec.Report = report.String()
	rec.Verdict = faults.Grade(rec.Report, rec.Active)
	s.saveInvestigation(rec)
	send("done", rec)
}

func (s *Server) saveInvestigation(rec Investigation) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = append(s.history, rec)
	if s.HistoryFile == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.HistoryFile), 0o755)
	f, err := os.OpenFile(s.HistoryFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(rec)
	f.Write(append(b, '\n'))
}

// LoadHistory reads earlier investigations so the scoreboard survives restarts.
func (s *Server) LoadHistory() {
	b, err := os.ReadFile(s.HistoryFile)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		var rec Investigation
		if json.Unmarshal([]byte(line), &rec) == nil && !rec.At.IsZero() {
			rec.Verdict = faults.Grade(rec.Report, rec.Active) // grading rules may have changed
			s.history = append(s.history, rec)
		}
	}
}

func (s *Server) investigations() []Investigation {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return append([]Investigation(nil), s.history...)
}
