# Scopes

A **scope** is a named partition of your memory. Each scope has its own
episode table, vector index, and optional remote git repository.

## Built-in scopes

| Scope      | Purpose                                                                    |
|------------|----------------------------------------------------------------------------|
| personal   | Private facts about you (default scope).                                   |
| project    | Facts tied to a specific codebase or engagement.                           |
| team       | Shared facts visible to collaborators.                                     |
| draft      | Private staging area. Records are invisible to recall until promoted.      |

The `draft` scope is the staging area for `remember --scope draft`. Draft records are excluded from all read surfaces (recall, getContext, entities, navigate) until you explicitly promote or discard them via `maz scope draft promote` / `maz scope draft discard`. Draft data never syncs to remotes.

## Configuring a scope remote

Add a `[sync.remotes.<scope>]` block in `config.toml`:

```toml
[sync.remotes.personal]
url         = "git@github.com:you/mazarbul-personal.git"
auth_method = "ssh"
```

## Encryption

By default scopes use **plaintext** `.jsonl` chunks so you can browse your
memory on GitHub. Opt in to encryption per scope:

```toml
[sync.remotes.personal]
encrypted = true
```

An encrypted scope requires a Watchword in your OS keychain. Set one with:

```
maz secret set watchword.personal
```

## Scope selection

Most commands accept `--scope` (or the `MAZ_SCOPE` env var) to target a
specific scope. When omitted, the command operates on the default scope
(`personal`).

## Documents

Documents are scope-aware just like facts, entities, and relations. Choose a
document's scope at creation:

```
maz doc new notes/roadmap.md --scope team     # visible to every team member
writeDocument(path=notes/idea.md, scope=draft) # private staging
```

Key rules:

- **Scope is fixed at creation.** `maz doc edit` (and the `editDocument` tool)
  never change a document's scope — they preserve it. A document IS an entity,
  so its scope, its companion entity, and its wikilink edges all share one
  partition.
- **Uniqueness is per scope.** The same vault-relative path can name one
  document in *each* scope: `notes/foo.md` in `personal` and `notes/foo.md` in
  `team` are two separate documents. Writing an existing path under a different
  scope mints a new document rather than moving the old one.
- **The vault is partitioned on disk** as `<vault-root>/<scope>/<path>` — e.g.
  `<root>/personal/notes/foo.md` and `<root>/team/notes/foo.md`. Editing a file
  under a scope directory ingests it back into that scope.
- **`team` requires membership** (else `E_PERMISSION`); **`draft`** documents
  stage privately and never sync.
- Bulk `maz doc import` of an external directory lands in `personal` for now.
