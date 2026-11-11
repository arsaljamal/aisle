// Package rag answers shopping questions from retrieved products only, and
// cites them, so every claim can be traced back to a catalogue entry.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/arsaljamal/aisle/internal/faults"
	"github.com/arsaljamal/aisle/internal/search"
	"github.com/arsaljamal/aisle/internal/store"
)

var (
	tracer = otel.Tracer("github.com/arsaljamal/aisle/internal/rag")
	meter  = otel.Meter("github.com/arsaljamal/aisle/internal/rag")
)

const system = `You are a shopping assistant for a home furniture store.
Answer ONLY from the numbered products given. Cite every product you mention as [P<id>], e.g. [P123].
If none of the products fit, say so plainly. Keep the answer under 120 words.`

// Chat is any OpenAI-compatible chat endpoint (Ollama, OpenAI, ...).
type Chat struct {
	BaseURL, Model, APIKey string
	HTTP                   *http.Client
}

type Answerer struct {
	Search *search.Searcher
	Chat   Chat
	Faults *faults.Set

	duration metric.Float64Histogram
	tokens   metric.Int64Counter
}

func New(s *search.Searcher, c Chat, f *faults.Set) (*Answerer, error) {
	a := &Answerer{Search: s, Chat: c, Faults: f}
	var err error
	if a.duration, err = meter.Float64Histogram("aisle.ask.duration", metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(.25, .5, 1, 2, 4, 8, 15, 30, 60)); err != nil {
		return nil, err
	}
	if a.tokens, err = meter.Int64Counter("aisle.llm.tokens",
		metric.WithDescription("Tokens used by the answer model")); err != nil {
		return nil, err
	}
	return a, nil
}

type Answer struct {
	Text       string
	Cited      []store.Hit
	Considered []store.Hit
}

func (a *Answerer) Ask(ctx context.Context, question string, n int) (ans Answer, err error) {
	ctx, span := tracer.Start(ctx, "rag.ask")
	start := time.Now()
	defer func() {
		status := "ok"
		if err != nil {
			status = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		a.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String("status", status)))
		span.End()
	}()

	hits, err := a.Search.Search(ctx, question, search.Hybrid, n)
	if err != nil {
		return Answer{}, err
	}
	ans.Considered = hits
	text, err := a.complete(ctx, Prompt(hits), question)
	if err != nil {
		return ans, err
	}
	ans.Text = text
	ans.Cited = Cited(text, hits)
	span.SetAttributes(attribute.Int("rag.considered", len(hits)), attribute.Int("rag.cited", len(ans.Cited)))
	return ans, nil
}

// Prompt lists the products the model may use.
func Prompt(hits []store.Hit) string {
	var b strings.Builder
	b.WriteString("Products:\n")
	for _, h := range hits {
		fmt.Fprintf(&b, "[P%d] %s (%s; %s; rating %.1f)\n", h.ID, h.Name, h.Class, h.Category, h.Rating)
	}
	return b.String()
}

var citeRe = regexp.MustCompile(`\[P(\d+)\]`)

// Cited returns the retrieved products the answer refers to, in order of
// first mention. Citations to products that were not retrieved are dropped.
func Cited(text string, hits []store.Hit) []store.Hit {
	byID := map[int64]store.Hit{}
	for _, h := range hits {
		byID[h.ID] = h
	}
	seen := map[int64]bool{}
	var out []store.Hit
	for _, m := range citeRe.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		if h, ok := byID[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, h)
		}
	}
	return out
}

func (a *Answerer) complete(ctx context.Context, products, question string) (string, error) {
	ctx, span := tracer.Start(ctx, "llm.chat")
	defer span.End()
	span.SetAttributes(attribute.String("gen_ai.request.model", a.Chat.Model))
	base := a.Chat.BaseURL
	if a.Faults.On(faults.LLMDown) {
		base = "http://127.0.0.1:1/v1" // nothing listens here
	}
	body, _ := json.Marshal(map[string]any{
		"model": a.Chat.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": products + "\nQuestion: " + question},
		},
		"temperature": 0.2,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.Chat.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.Chat.APIKey)
	}
	hc := a.Chat.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		err = fmt.Errorf("answer model: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("answer model: status %d: %.200s", resp.StatusCode, raw)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("answer model: unreadable reply: %.200s", raw)
	}
	model := attribute.String("gen_ai.request.model", a.Chat.Model)
	a.tokens.Add(ctx, out.Usage.PromptTokens, metric.WithAttributes(model, attribute.String("kind", "input")))
	a.tokens.Add(ctx, out.Usage.CompletionTokens, metric.WithAttributes(model, attribute.String("kind", "output")))
	span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", out.Usage.PromptTokens),
		attribute.Int64("gen_ai.usage.output_tokens", out.Usage.CompletionTokens))
	return stripThinking(out.Choices[0].Message.Content), nil
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

func stripThinking(s string) string { return strings.TrimSpace(thinkRe.ReplaceAllString(s, "")) }
