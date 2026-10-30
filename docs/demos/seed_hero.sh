#!/usr/bin/env bash
# Normalize the hero graph to a clean, canonical shape right after `maz remember`.
#
# Local LLM extraction (qwen2.5:7b) is genuinely powerful but non-deterministic:
# run to run it varies which entities it captures, the predicate it picks, the
# direction of an edge, and even the exact name ("Gandalf" vs "Gandalf the Grey").
# The hero GIF has to read perfectly every render, so we let `remember` do the
# real in-process extraction (the episode, embeddings, and entities it produces
# are all real and drive recall), then rewrite the edges on the inspected nodes to
# the canonical ones. We relate by entity UID, never by name, so a duplicate or a
# renamed entity can never make `relate` ambiguous.
#
# Expects MAZ_DATA_DIR (already initialised + a remember run) and maz on PATH.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH="$PWD/bin:$PATH"
M=maz

# uid_for <substring>: first non-self entity whose display name contains it.
uid_for() {
	$M entities --json 2>/dev/null | python3 -c "
import json, sys
sub = sys.argv[1].lower()
d = json.load(sys.stdin)
m = [e for e in d.get('entities', []) if sub in e['displayName'].lower() and e['uid'] != 'self']
print('entities/' + m[0]['uid'] if m else '')
" "$1"
}

# 1. Forget every relation the extractor placed (enumerated via each entity's
#    neighbors). Leaves the entities and the episode intact.
$M entities --json 2>/dev/null | python3 -c "
import json, sys
d = json.load(sys.stdin)
print('\n'.join(e['uid'] for e in d.get('entities', []) if e['uid'] != 'self'))
" | while read -r euid; do
	[ -n "$euid" ] || continue
	$M neighbors "entities/$euid" --json 2>/dev/null | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    for n in d.get('neighbors', []):
        uid = n.get('via', {}).get('uid')
        if uid:
            print(uid)
except Exception:
    pass
"
done | sort -u | while read -r r; do
	[ -n "$r" ] && $M forget "relations/$r" >/dev/null 2>&1
done

# 2. Resolve canonical nodes by substring; create only what extraction missed.
frodo=$(uid_for "frodo")
gandalf=$(uid_for "gandalf")
sam=$(uid_for "samwise")
ring=$(uid_for "ring")
[ -z "$ring" ] && { $M entity create object "The One Ring" >/dev/null 2>&1; ring=$(uid_for "ring"); }
[ -z "$frodo" ] && { $M entity create person "Frodo Baggins" >/dev/null 2>&1; frodo=$(uid_for "frodo"); }
[ -z "$gandalf" ] && { $M entity create person "Gandalf the Grey" >/dev/null 2>&1; gandalf=$(uid_for "gandalf"); }
[ -z "$sam" ] && { $M entity create person "Samwise Gamgee" >/dev/null 2>&1; sam=$(uid_for "samwise"); }

# 3. Canonical predicates + relations, addressed by UID so they never go ambiguous.
$M schema predicates add bears --description "carries an object of power" >/dev/null 2>&1
$M schema predicates add guides --description "leads and protects" >/dev/null 2>&1
$M schema predicates add serves --description "loyally serves" >/dev/null 2>&1
[ -n "$frodo" ] && [ -n "$ring" ] && $M relate "$frodo" bears "$ring" >/dev/null 2>&1
[ -n "$gandalf" ] && [ -n "$frodo" ] && $M relate "$gandalf" guides "$frodo" >/dev/null 2>&1
[ -n "$sam" ] && [ -n "$frodo" ] && $M relate "$sam" serves "$frodo" >/dev/null 2>&1
echo "normalized"
