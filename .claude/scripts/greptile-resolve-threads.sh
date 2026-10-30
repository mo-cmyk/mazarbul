#!/usr/bin/env bash
# greptile-resolve-threads.sh , Resolve Greptile review threads in the GitHub UI.
#
# Usage:
#   .claude/scripts/greptile-resolve-threads.sh list <PR>          , print Greptile threads (JSON, one per line)
#   .claude/scripts/greptile-resolve-threads.sh resolve <THREAD>   , mark a thread resolved
#   .claude/scripts/greptile-resolve-threads.sh verify <PR>        , exit 1 if any Greptile thread unresolved
#
# The /greptile skill calls `list` to enumerate, applies fixes + replies, then
# calls `resolve` on each addressed thread. Phase 4 of the skill MUST end with
# `verify` , if any Greptile thread is still isResolved=false, the skill exits
# non-zero so the babysit loop knows to keep iterating.
#
# Greptile's bot logins:
#   - greptile-apps[bot]  , inline review-thread comments
#   - greptile-apps       , top-level reviews / comments
# We filter case-insensitively on "greptile" to catch both.

set -euo pipefail

cmd="${1:?usage: $0 <list|resolve|verify> ...}"

repo_parts() {
  gh repo view --json owner,name -q '"\(.owner.login) \(.name)"'
}

# Print every Greptile review thread on the PR, paginated. Each line is one JSON
# object: {threadId, isResolved, path, line, body, commentId}. We must paginate
# because PRs in active review can easily exceed 100 review threads, and a
# bounded `first:100` query would silently drop threads , making `verify` lie.
list_threads() {
  local pr="$1" owner name cursor='null' page
  read -r owner name <<<"$(repo_parts)"
  while :; do
    page=$(gh api graphql -f query='
      query($owner:String!,$repo:String!,$pr:Int!,$cursor:String){
        repository(owner:$owner,name:$repo){pullRequest(number:$pr){
          reviewThreads(first:100, after:$cursor){
            pageInfo{ hasNextPage endCursor }
            nodes{
              id isResolved
              comments(first:10){nodes{ id databaseId author{login} body path line }}
            }
          }
        }}
      }' -F owner="$owner" -F repo="$name" -F pr="$pr" -F cursor="$cursor")

    echo "$page" | jq -c '.data.repository.pullRequest.reviewThreads.nodes[] |
      select(.comments.nodes[0].author.login | test("greptile"; "i")) |
      {threadId: .id,
       isResolved,
       path: .comments.nodes[0].path,
       line: .comments.nodes[0].line,
       body: .comments.nodes[0].body,
       commentId: .comments.nodes[0].databaseId}'

    has_next=$(echo "$page" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage')
    [ "$has_next" = "true" ] || break
    cursor=$(echo "$page" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor')
    # -F type-coerces "null"/"true"/numbers, else passes as string , perfect for cursors.
  done
}

resolve_thread() {
  local thread="$1"
  gh api graphql -f query='
    mutation($id:ID!){ resolveReviewThread(input:{threadId:$id}){thread{isResolved}} }
  ' -f id="$thread" --jq '.data.resolveReviewThread.thread.isResolved'
}

# Count unresolved Greptile threads across ALL pages, then fail loud if >0.
verify_all_resolved() {
  local pr="$1" owner name cursor='null' page unresolved=0 page_unresolved
  read -r owner name <<<"$(repo_parts)"
  while :; do
    page=$(gh api graphql -f query='
      query($owner:String!,$repo:String!,$pr:Int!,$cursor:String){
        repository(owner:$owner,name:$repo){pullRequest(number:$pr){
          reviewThreads(first:100, after:$cursor){
            pageInfo{ hasNextPage endCursor }
            nodes{
              isResolved
              comments(first:1){nodes{ author{login} }}
            }
          }
        }}
      }' -F owner="$owner" -F repo="$name" -F pr="$pr" -F cursor="$cursor")

    page_unresolved=$(echo "$page" | jq '[.data.repository.pullRequest.reviewThreads.nodes[] |
      select(.comments.nodes[0].author.login | test("greptile"; "i")) |
      select(.isResolved == false)] | length')
    unresolved=$((unresolved + page_unresolved))

    has_next=$(echo "$page" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage')
    [ "$has_next" = "true" ] || break
    cursor=$(echo "$page" | jq -r '.data.repository.pullRequest.reviewThreads.pageInfo.endCursor')
  done

  if [ "$unresolved" -gt 0 ]; then
    echo "VERIFY FAIL: $unresolved unresolved Greptile thread(s) on PR #$pr" >&2
    exit 1
  fi
  echo "VERIFY OK: all Greptile threads resolved on PR #$pr"
}

case "$cmd" in
  list)    list_threads "${2:?usage: list <PR>}" ;;
  resolve) resolve_thread "${2:?usage: resolve <THREAD-ID>}" ;;
  verify)  verify_all_resolved "${2:?usage: verify <PR>}" ;;
  *)       echo "usage: $0 <list|resolve|verify> ..." >&2; exit 1 ;;
esac
