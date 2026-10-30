# Watchword — Encryption Key Management

A **Watchword** is the passphrase used to seal encrypted memory chunks.
Mazarbul derives a 256-bit XChaCha20-Poly1305 key from the Watchword using
Argon2id.

## Setting a Watchword

```bash
maz secret set watchword.personal
```

You will be prompted to enter and confirm the passphrase. It is stored in
the OS keychain (macOS Keychain, libsecret on Linux, Windows Credential
Manager).

## Rotating a Watchword

A Watchword rotation re-seals all existing chunks with the new key. This
is a destructive operation on the local copy; push before rotating.

```bash
maz sync push                    # ensure remote is up to date
maz secret set watchword.personal  # set the new passphrase
maz rebuild                      # re-seal all chunks
```

## BIP-39 seed words

When `maz init` creates an encrypted scope it generates a BIP-39 mnemonic
as a human-readable backup of the derived key. Store the mnemonic offline.

Recover from mnemonic:

```bash
maz mellon --recover
```

## Multiple scopes

Each scope has its own Watchword:

```bash
maz secret set watchword.team
maz secret set watchword.project
```
