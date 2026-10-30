# Moria — The Local Memory Store

*"They have a Cave Troll."*

Moria is the name of Mazarbul's local data store — the directory that
contains SQLite, the vector index, the knowledge graph, and sealed chunks.
By default it lives at `~/.local/share/mazarbul` on Linux and
`~/Library/Application Support/mazarbul` on macOS.

## Override the data directory

```bash
export MAZ_DATA_DIR=/Volumes/fast-drive/mazarbul
maz remember "stored on the fast drive"
```

Or per-invocation:

```bash
maz --data-dir /tmp/test-book init --yes --provider ollama --chat-model qwen2.5:7b
```

## Directory layout

```
$MAZ_DATA_DIR/
├── config.toml        # user-edited configuration
├── state.toml         # TUI-managed overrides
├── mazarbul.db        # SQLite episodes + FTS5 index
├── mazarbul.vec       # sqlite-vec vector embeddings
├── graph.ladybug      # LadybugDB knowledge graph
├── sync/              # git repository for sealed chunks
│   └── personal/
│       └── chunks/
└── logs/
    └── daemon.log
```

## Multiple books

Each `--data-dir` is an independent memory book. You can maintain separate
books for work and personal use:

```bash
maz --data-dir ~/mazarbul-work  remember "work context"
maz --data-dir ~/mazarbul-home  remember "home context"
```
