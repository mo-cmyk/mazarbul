# Four-Layer Storage

Mazarbul stores knowledge in four complementary layers that each serve a
different retrieval mode.

## Layer 1: SQLite + FTS5 (episodes)

Every `maz remember` writes a raw episode row into SQLite. The FTS5 full-text
index powers the BM25 keyword lane of `maz recall`.

## Layer 2: sqlite-vec (vector embeddings)

When an embed provider is configured, the embedder writes a float32 vector
for each episode alongside its SQLite row. The cosine-similarity lane of
`maz recall` uses this index.

## Layer 3: LadybugDB (knowledge graph)

The extractor parses entities (people, places, things) and relations
(works_at, located_in, knows) from each episode and writes them to a
LadybugDB property graph. Graph-expansion (`maz traverse`) and the
2-hop neighbourhood lane of `maz recall` use this layer.

## Layer 4: Git (sealed chunks)

The sync layer seals episodes into `.jsonl` (plaintext) or `.maz`
(encrypted) chunk files and pushes them to a configured remote Git
repository. This layer enables cross-device sync and off-site backup.

## Precedence in recall

The four lanes are fused by Reciprocal Rank Fusion (RRF). Configure
weights in `[recall]` in `config.toml`:

```toml
[recall]
graph_weight     = 1.0
name_emb_weight  = 1.0
rrf_min_score    = 0.005
```

See `maz help scopes` for the per-scope privacy model.
