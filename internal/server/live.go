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
	"strconv"
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
	// The stream speaks the Drill Down viewer's protocol: every event is a
	// "step" holding one JSON object; "end" closes it.
	step := func(ev []byte) {
		fmt.Fprintf(w, "event: step\ndata: %s\n\n", ev)
		flusher.Flush()
	}
	rec := Investigation{At: time.Now(), Alert: key, Active: s.Faults.Active()}
	args := append(append([]string(nil), s.Investigator[1:]...), alert)
	cmd := exec.CommandContext(r.Context(), s.Investigator[0], args...)
	cmd.Dir = s.InvestigatorDir
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		step(hostEvent("final", map[string]any{"message": "could not start the investigator: " + err.Error()}))
		fmt.Fprint(w, "event: end\ndata: {}\n\n")
		return
	}
	go io.Copy(io.Discard, stdout) // the report also arrives as a "final" event
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sentContext := false
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var ev struct {
			Kind         string `json:"kind"`
			Turn         int    `json:"turns"`
			Stop         string `json:"stop"`
			Message      string `json:"message"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Kind == "" {
			continue // tool server logs, not investigator steps
		}
		switch ev.Kind {
		case "tool_call":
			rec.Queries++
		case "gate":
			rec.Nudges++
		case "summary":
			rec.Turns, rec.Stop, rec.Tokens = ev.Turn, ev.Stop, ev.InputTokens+ev.OutputTokens
		case "final":
			rec.Report = ev.Message
		}
		rec.Steps = append(rec.Steps, line)
		step(line)
		if ev.Kind == "meta" && !sentContext {
			sentContext = true
			c := hostEvent("context", map[string]any{"truth": truthNote(rec.Active)})
			rec.Steps = append(rec.Steps, c)
			step(c)
		}
	}
	_ = cmd.Wait()
	rec.Seconds = time.Since(rec.At).Seconds()
	rec.Verdict = faults.Grade(rec.Report, rec.Active)
	v := verdictEvent(rec)
	rec.Steps = append(rec.Steps, v)
	step(v)
	s.saveInvestigation(rec)
	fmt.Fprint(w, "event: end\ndata: {}\n\n")
	flusher.Flush()
}

func hostEvent(kind string, fields map[string]any) []byte {
	fields["kind"] = kind
	fields["at"] = time.Now().UnixMilli()
	b, _ := json.Marshal(fields)
	return b
}

func truthNote(active []string) string {
	if len(active) == 0 {
		return "No break switch is on: this is a false-alarm test."
	}
	return "Break switch actually on (the investigator is not told): <b>" + strings.Join(active, ", ") + "</b>"
}

func verdictEvent(rec Investigation) []byte {
	return hostEvent("verdict", map[string]any{"verdict": rec.Verdict,
		"note": "graded automatically against the switch that was on (a rough guide; check the report)"})
}

// replay streams a stored investigation at its recorded pace divided by speed.
func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	i, err := strconv.Atoi(r.PathValue("i"))
	hist := s.investigations()
	if err != nil || i < 0 || i >= len(hist) {
		http.NotFound(w, r)
		return
	}
	speed, _ := strconv.ParseFloat(r.URL.Query().Get("speed"), 64)
	if speed <= 0 {
		speed = 3
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rec := hist[i]
	steps := rec.Steps
	if len(steps) > 0 && !strings.Contains(string(steps[0]), `"meta"`) {
		// Recorded before meta events existed: add one so the page can start.
		meta := hostEvent("meta", map[string]any{"task": Alerts[rec.Alert], "components": s.Components})
		steps = append([]json.RawMessage{meta, hostEvent("context", map[string]any{"truth": truthNote(rec.Active)})}, steps...)
		steps = append(steps, hostEvent("final", map[string]any{"message": rec.Report}), verdictEvent(rec))
	}
	var last int64
	for _, ev := range steps {
		var at struct {
			At int64 `json:"at"`
		}
		_ = json.Unmarshal(ev, &at)
		if last > 0 && at.At > last {
			select {
			case <-time.After(min(time.Duration(float64(at.At-last)/speed)*time.Millisecond, 3*time.Second)):
			case <-r.Context().Done():
				return
			}
		}
		if at.At > 0 {
			last = at.At
		}
		fmt.Fprintf(w, "event: step\ndata: %s\n\n", ev)
		flusher.Flush()
	}
	fmt.Fprint(w, "event: end\ndata: {}\n\n")
	flusher.Flush()
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
