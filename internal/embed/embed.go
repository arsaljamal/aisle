// Package embed turns text into vectors through any OpenAI-compatible
// embeddings endpoint (Ollama, OpenAI, LM Studio, ...).
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/arsaljamal/aisle/internal/faults"
)

var tracer = otel.Tracer("github.com/arsaljamal/aisle/internal/embed")

type Client struct {
	BaseURL string // e.g. http://localhost:11434/v1
	Model   string
	APIKey  string
	HTTP    *http.Client
	Faults  *faults.Set
	// Prefixes some models (nomic-embed-text) need to tell queries from documents.
	QueryPrefix, DocPrefix string
}

func (c *Client) Query(ctx context.Context, text string) ([]float32, error) {
	ctx, span := tracer.Start(ctx, "embed.query")
	defer span.End()
	if c.Faults.On(faults.SlowEmbed) {
		time.Sleep(1200*time.Millisecond + time.Duration(rand.IntN(300))*time.Millisecond)
	}
	v, err := c.call(ctx, []string{c.QueryPrefix + text})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if c.Faults.On(faults.ModelMismatch) {
		return scramble(v[0]), nil
	}
	return v[0], nil
}

func (c *Client) Documents(ctx context.Context, texts []string) ([][]float32, error) {
	ctx, span := tracer.Start(ctx, "embed.documents")
	defer span.End()
	span.SetAttributes(attribute.Int("embed.batch", len(texts)))
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = c.DocPrefix + t
	}
	return c.call(ctx, in)
}

// scramble stands in for "a different model": same length, unrelated space.
func scramble(v []float32) []float32 {
	out := make([]float32, len(v))
	for i := range v {
		out[i] = v[(i*7+3)%len(v)]
	}
	return out
}

func (c *Client) call(ctx context.Context, input []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": c.Model, "input": input})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		slog.ErrorContext(ctx, "embedding call failed", "status", resp.StatusCode, "model", c.Model)
		return nil, fmt.Errorf("embed: status %d: %.200s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(out.Data) != len(input) {
		return nil, fmt.Errorf("embed: asked for %d vectors, got %d", len(input), len(out.Data))
	}
	vecs := make([][]float32, len(input))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embed: bad index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
