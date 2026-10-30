# Recall

`maz recall <query>` retrieves the most relevant episodes from your memory
using a hybrid four-lane retrieval strategy.

## Retrieval lanes

| Lane          | Method                       | Weight knob            |
|---------------|------------------------------|------------------------|
| Vector        | Cosine similarity (ANN)      | 1.0 (fixed)            |
| FTS5          | BM25 keyword search          | 1.0 (fixed)            |
| Graph 1-hop   | Entity neighbourhood         | `graph_weight`         |
| Name-embed    | Entity name cosine sim       | `name_emb_weight`      |

Results are fused by Reciprocal Rank Fusion (RRF) and optionally re-ranked
by recency and Maximal Marginal Relevance (MMR).

## Tuning

Edit `[recall]` in `config.toml` or `state.toml`:

```toml
[recall]
mmr_lambda           = 0.7     # 0 = max diversity, 1 = max relevance
recency_boost        = 0.15    # 0 = off, 0.15 = recommended
recency_halflife_days = 30
rrf_min_score        = 0.005
```

## Temporal filter (--at)

`--at` is a **valid-time** filter, not a time-travel cursor for episode
visibility. Passing `--at 2025-06-01T00:00:00Z` narrows entity-fact lookups
to those whose validity window covers that instant. It does **not** hide
episodes that were recorded after that timestamp — episodes are stored as
immutable events and carry no validity window. Use `--at` to answer
"what did we know about this entity at that point in time?", not to replay
what episodes existed then.

## Output formats

```bash
maz recall "project deadline"                 # human text
maz recall "project deadline" --output json   # JSON array
maz recall "project deadline" --output ndjson # one JSON object per line
maz recall "project deadline" --limit 10      # cap results
```
