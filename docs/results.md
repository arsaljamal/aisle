# Results

All numbers: WANDS (42,994 products, 480 shopper queries, 233,448 human labels), top 10 results,
gains Exact = 2, Partial = 1. Embeddings: `nomic-embed-text` (768 dims) via Ollama on an M-series
MacBook. Postgres 17 + pgvector, HNSW (m 16, ef_construction 64, ef_search 100). Latency is measured
in-process by `aisle eval`, one query at a time.

## Relevance

| Mode | NDCG@10 | Exact precision@10 | MRR (first exact) | Zero results | p50 | p95 |
|---|---|---|---|---|---|---|
| Keyword | 0.689 | 0.356 | 0.532 | 1 | 27 ms | 108 ms |
| Vector | 0.697 | 0.364 | 0.547 | 0 | 11 ms | 15 ms |
| **Hybrid (RRF)** | **0.738** | **0.383** | **0.569** | 0 | 29 ms | 105 ms |

## What moved the numbers

| Change | Keyword NDCG | Hybrid NDCG | Why |
|---|---|---|---|
| Baseline: match any word (OR), weighted full text | 0.599 | 0.710 | Long queries still return results, but partial matches crowd the top. |
| Rank products containing every word first | **0.689** (+0.090) | **0.738** (+0.028) | Keeps OR recall, restores AND precision. Hybrid now beats vector on every measure. |

Nothing was tuned per query or fitted to the labels: both changes are general rules, applied once.

Reproduce: `make demo` (or `./bin/aisle eval`), which also writes `results/eval.json`.
