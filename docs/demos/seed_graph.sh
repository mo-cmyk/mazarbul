#!/usr/bin/env bash
# Seed a rich, deterministic Lord of the Rings graph for the Claude demo: typed
# entities + relations + a fact with history. No LLM, so it is fast and identical
# every run. `maz relate` accepts display names, so the graph wires together by
# name with no uid bookkeeping.
#
# Expects MAZ_DATA_DIR (already `maz init`-ed) and maz on PATH.
#
# This graph is deterministic (no LLM), so a failed entity-create or relate is a
# bug, not expected variance: run under -e to abort loudly rather than leave a
# half-seeded graph for the demo. The genuinely-optional steps (schema add, the
# fact-history writes) keep their explicit `|| true`.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH="$PWD/bin:$PATH"
M=maz

$M schema kinds add artifact --display-name "Artifact" --description "An object of power" >/dev/null 2>&1 || true
$M schema predicates add bears --description "carries an object of power" >/dev/null 2>&1 || true

$M entity create person "Frodo Baggins" --attributes '{"race":"hobbit","role":"Ring-bearer"}' >/dev/null
$M entity create person "Samwise Gamgee" --attributes '{"race":"hobbit","role":"gardener"}' >/dev/null
$M entity create person "Gandalf" --attributes '{"race":"wizard","title":"the Grey"}' >/dev/null
$M entity create person "Aragorn" --attributes '{"race":"man","title":"heir of Isildur"}' >/dev/null
$M entity create person "Boromir" --attributes '{"race":"man","of":"Gondor"}' >/dev/null
$M entity create person "Legolas" --attributes '{"race":"elf"}' >/dev/null
$M entity create person "Gimli" --attributes '{"race":"dwarf"}' >/dev/null
$M entity create person "Saruman" --attributes '{"race":"wizard","title":"the White"}' >/dev/null
$M entity create person "Sauron" --attributes '{"race":"maia","title":"the Dark Lord"}' >/dev/null
$M entity create artifact "The One Ring" --attributes '{"power":"invisibility and corruption","forged_in":"Mount Doom"}' >/dev/null
$M entity create organization "The Fellowship of the Ring" --attributes '{"purpose":"destroy the One Ring"}' >/dev/null

$M relate "Frodo Baggins" bears "The One Ring" >/dev/null
$M relate "Frodo Baggins" mentored_by "Gandalf" >/dev/null
$M relate "Samwise Gamgee" sworn_to "Frodo Baggins" >/dev/null
$M relate "Sauron" seeks "The One Ring" >/dev/null
$M relate "Saruman" serves "Sauron" >/dev/null
for m in "Frodo Baggins" "Samwise Gamgee" "Gandalf" "Aragorn" "Boromir" "Legolas" "Gimli"; do
	$M relate "$m" member_of "The Fellowship of the Ring" >/dev/null
done

# A fact with history: the Ring-bearer's road advances (older value superseded, kept).
$M fact set "Frodo Baggins" location "the Shire" >/dev/null 2>&1 || true
$M fact set "Frodo Baggins" location "Rivendell" >/dev/null 2>&1 || true
echo "seeded"
