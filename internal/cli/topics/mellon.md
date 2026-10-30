# Mellon — Speak, Friend, and Enter

*"Speak, friend, and enter."*

`maz mellon` is the session watchword gate. It verifies that the Watchword
stored in the OS keychain matches the one sealed into your memory book.
Think of it as a sanity check that your keys are in order before a long
session.

## Usage

```bash
maz mellon           # interactive TTY check
maz mellon --output json | jq .ok   # scripted check
```

## What it checks

1. Loads the Watchword from the OS keychain (`mazarbul:watchword`).
2. Verifies it can decrypt the most recent sealed chunk for the default scope.
3. Prints `ok: true` on success; exits with code 1 on mismatch.

## Key management

Set or rotate the Watchword:

```bash
maz secret set watchword.personal
```

Export the Watchword for backup:

```bash
maz secret get watchword.personal
```

See `maz help watchword` for the full key-management reference.
