// Package store keeps products in Postgres. One table serves both keyword
// search (a weighted full-text column) and vector search (pgvector + HNSW).
package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
	"go.opentelemetry.io/otel"

	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/faults"
)

var tracer = otel.Tracer("github.com/arsaljamal/aisle/internal/store")

type Store struct {
	pool   *pgxpool.Pool
	Faults *faults.Set
	slot   sync.Mutex // used only by the db-pool fault
}

type Hit struct {
	ID       int64
	Name     string
	Class    string
	Category string
	Rating   float32
	Score    float32
}

func Open(ctx context.Context, dsn string, efSearch int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTrimSQLInSpanName())
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		if err := pgxvec.RegisterTypes(ctx, c); err != nil {
			return err
		}
		_, err := c.Exec(ctx, fmt.Sprintf("SET hnsw.ef_search = %d", efSearch))
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w (is `make up` running?)", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool exposes connection stats for metrics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate needs the vector extension before the first connection registers
// its types, so it runs on a plain connection.
func Migrate(ctx context.Context, dsn string, dims int) error {
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w (is `make up` running?)", err)
	}
	defer c.Close(ctx)
	_, err = c.Exec(ctx, fmt.Sprintf(`
CREATE EXTENSION IF NOT EXISTS vector;
CREATE TABLE IF NOT EXISTS products (
  id          bigint PRIMARY KEY,
  name        text NOT NULL,
  class       text NOT NULL DEFAULT '',
  category    text NOT NULL DEFAULT '',
  description text NOT NULL DEFAULT '',
  rating      real NOT NULL DEFAULT 0,
  reviews     int  NOT NULL DEFAULT 0,
  doc tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('english', name), 'A') ||
    setweight(to_tsvector('english', class || ' ' || category), 'B') ||
    setweight(to_tsvector('english', left(description, 2000)), 'C')
  ) STORED,
  embedding vector(%d)
);
CREATE INDEX IF NOT EXISTS products_doc_idx ON products USING gin (doc);
`, dims))
	return err
}

// IndexVectors builds the HNSW index once embeddings exist (faster than
// maintaining it during the bulk load).
func (s *Store) IndexVectors(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS products_embedding_idx
  ON products USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)`)
	return err
}

func (s *Store) Load(ctx context.Context, products []catalog.Product) (int64, error) {
	rows := make([][]any, len(products))
	for i, p := range products {
		rows[i] = []any{p.ID, p.Name, p.Class, p.Category, p.Description, p.Rating, p.Reviews}
	}
	return s.pool.CopyFrom(ctx, pgx.Identifier{"products"},
		[]string{"id", "name", "class", "category", "description", "rating", "reviews"},
		pgx.CopyFromRows(rows))
}

func (s *Store) Truncate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "TRUNCATE products")
	return err
}

type Pending struct {
	ID  int64
	Doc string
}

// Unembedded returns products still missing a vector, so embedding can resume.
func (s *Store) Unembedded(ctx context.Context, limit int, maxDesc int) ([]Pending, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, class, category, left(description, $2)
  FROM products WHERE embedding IS NULL ORDER BY id LIMIT $1`, limit, maxDesc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p catalog.Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Class, &p.Category, &p.Description); err != nil {
			return nil, err
		}
		out = append(out, Pending{ID: p.ID, Doc: p.Document(maxDesc)})
	}
	return out, rows.Err()
}

func (s *Store) SetEmbeddings(ctx context.Context, ids []int64, vecs [][]float32) error {
	b := &pgx.Batch{}
	for i, id := range ids {
		b.Queue("UPDATE products SET embedding = $1 WHERE id = $2", pgvector.NewVector(vecs[i]), id)
	}
	return s.pool.SendBatch(ctx, b).Close()
}

func (s *Store) Counts(ctx context.Context) (total, embedded int64, err error) {
	err = s.pool.QueryRow(ctx, "SELECT count(*), count(embedding) FROM products").Scan(&total, &embedded)
	return
}

// guard applies the db-pool fault: every query waits for one shared slot.
func (s *Store) guard(ctx context.Context) func() {
	if !s.Faults.On(faults.DBPool) {
		return func() {}
	}
	_, span := tracer.Start(ctx, "db.pool.acquire")
	s.slot.Lock()
	span.End()
	return func() {
		time.Sleep(time.Duration(80+rand.IntN(60)) * time.Millisecond)
		s.slot.Unlock()
	}
}

// Keyword matches any query word (OR), so long shopper queries still return
// results, but products that contain every word rank first.
func (s *Store) Keyword(ctx context.Context, query string, limit int) ([]Hit, error) {
	ctx, span := tracer.Start(ctx, "store.keyword")
	defer span.End()
	defer s.guard(ctx)()
	return s.hits(ctx, `
SELECT id, name, class, category, rating,
       (ts_rank_cd(doc, anyq, 32) + CASE WHEN doc @@ allq THEN 1 ELSE 0 END)::real AS score
FROM products,
     (SELECT plainto_tsquery('english', $1) AS allq,
             nullif(replace(plainto_tsquery('english', $1)::text, '&', '|'), '')::tsquery AS anyq) x
WHERE anyq IS NOT NULL AND doc @@ anyq
ORDER BY score DESC, id
LIMIT $2`, query, limit)
}

func (s *Store) Vector(ctx context.Context, vec []float32, limit int) ([]Hit, error) {
	ctx, span := tracer.Start(ctx, "store.vector")
	defer span.End()
	defer s.guard(ctx)()
	return s.hits(ctx, `
SELECT id, name, class, category, rating, (1 - (embedding <=> $1))::real AS score
FROM products
WHERE embedding IS NOT NULL
ORDER BY embedding <=> $1
LIMIT $2`, pgvector.NewVector(vec), limit)
}

func (s *Store) hits(ctx context.Context, sql string, args ...any) ([]Hit, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.Name, &h.Class, &h.Category, &h.Rating, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
