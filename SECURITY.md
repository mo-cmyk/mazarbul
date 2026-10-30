# Security policy

## Reporting a vulnerability

**Please do not file a public GitHub issue for security problems.**

The preferred channel is **GitHub's private vulnerability reporting**: navigate to the repo's Security tab and click "Report a vulnerability", or go directly to:

> https://github.com/mo-cmyk/mazarbul/security/advisories/new

Please include:

- A clear description of the issue and its impact.
- Steps to reproduce (proof-of-concept code, sample input, or a minimal repro).
- The version or commit SHA you tested against.
- Your name or handle for credit when the fix ships (optional).

## Coordinated-disclosure timeline

These are goals for a solo maintainer, not contractual SLAs.

| Stage | Target |
|---|---|
| Acknowledgement | ≤ 72 hours from receipt |
| Triage and severity assignment | ≤ 7 days from acknowledgement |
| Fix — Critical / High | ≤ 14 days from triage |
| Fix — Medium | ≤ 60 days from triage |
| Fix — Low | Best effort; may ship with next regular release |
| Max embargo before coordinated public disclosure | 90 days from acknowledgement |

If a fix requires deeper architectural work or an upstream dependency patch, we will communicate a revised timeline directly with the reporter and coordinate public disclosure via a published GHSA advisory once a fix is available.

## Severity rubric (CVSS v3.1; CVSS v4.0 where applicable)

| Rating | CVSS score | Fix urgency |
|---|---|---|
| Critical | 9.0 – 10.0 | Drop everything; hotfix ASAP |
| High | 7.0 – 8.9 | Priority fix in the next short cycle |
| Medium | 4.0 – 6.9 | Scheduled fix; may bundle with a feature release |
| Low | 0.1 – 3.9 | Best-effort; tracked and addressed opportunistically |

## What is in scope

- The `maz` binary (`cmd/maz/`) and all packages under `internal/`.
- The **MCP HTTP and Unix-socket transports** — the local daemon's network surface, including bearer-token validation and any unauthenticated endpoints.
- Default plugin manifests and the plugin install pipeline shipped from this repository.
- The official MCP tool definitions exposed by `maz mcp serve`.
- The default sync transport (git CRDT log) and its encrypted-scope key management.
- Documented configuration and state file formats (`config/*.yaml`, event-log JSONL, theme TOML).

## What is out of scope

- Third-party plugins and connectors not maintained in this repository.
- User-authored themes loaded via `--theme=<path>`.
- Attacks that require physical access to, or root/admin privileges on, the machine already running `maz`.
- Local denial-of-service against a single-user install (memory exhaustion via crafted input, etc.). We will still fix these, but they do not warrant private coordinated disclosure.
- Social-engineering attacks targeting the user directly.

## Supported versions

Mazarbul is pre-release. Only the current `main` branch receives fixes.

Once tagged releases exist, the support window will be documented here; the expected policy is the latest minor release plus the previous one.

## Public credit and acknowledgements

Reporters are credited by name (or handle) in the GHSA advisory and in the release notes that ship the fix. Anonymity is fully respected on request — just say so when you report.
