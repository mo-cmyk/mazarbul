# Sync

`maz sync` replicates your memory to a remote Git repository so you can
access it from multiple machines and keep an off-site backup.

## Quick start

```bash
# Point a scope at a remote repository:
maz config set sync.remotes.personal.url git@github.com:you/mazarbul.git

# Push immediately:
maz sync push

# Pull on another machine:
maz sync pull
```

## How it works

1. The **sealer** batches episodes into chunk files (`.jsonl` for plaintext,
   `.maz` for encrypted) and commits them to a local git repo inside
   `${DATA_DIR}/sync/`.
2. `maz sync push` runs `git push` to the configured remote.
3. `maz sync pull` runs `git pull` and lands new chunk files under
   `sync/<scope>/chunks/`. The running daemon picks them up and applies
   them to the SQLite, vector, and graph layers. In `--daemon off` mode
   the chunks are fetched but not indexed until a daemon applies them
   (run `maz rebuild` to index manually after a daemonless pull).

## Auto-push

Enable background push in `config.toml`:

```toml
[sync]
auto_push        = true
coalesce_seconds = 30
```

## Chunk format

Plaintext chunks are newline-delimited JSON (`.jsonl`), one episode per line.
They are human-readable and diff-friendly on GitHub.

Encrypted chunks are sealed with XChaCha20-Poly1305 using the scope's
Watchword. See `maz help watchword` for key management.
