# Repository audit (stage 07)

Date: 2026-10-22. Scope: working tree and all 18 commits on `main`.

| Check | Method | Result |
| --- | --- | --- |
| Secrets in tree and history | `make secret-scan` (gitleaks, tree + git history) | no leaks found |
| Known vulnerabilities | `govulncheck ./...` | 0 affecting the code. GO-2026-5932 (`golang.org/x/crypto/openpgp`, unmaintained, no fix) is in a required module; the code does not call it. Reported by CI as informational |
| Dependency licenses | `go-licenses csv ./...` | 29 Apache-2.0, 21 BSD-3, 2 BSD-2, 19 MIT, 1 Unlicense. `segmentio/asm` reports "Unknown" but its LICENSE is MIT No Attribution. `cyphar/filepath-securejoin` is BSD-3 with MPL-2.0 file-level terms; compatible when used unmodified. No GPL/AGPL. |
| Private hostnames / IPs | grep for RFC1918 ranges and hostnames in tracked files | none; placeholders `memory.example.com`, `*.localtest.me` only |
| Personal data | grep | commit author name and email (`tyler@lafronz.com`) are in git history; the author confirmed it is a public address (2026-10-22) |
| Internal-only references | grep | Local absolute paths and references to the author's personal cluster and Tailscale network were removed from `prompts/`, `proposal.md`, `review*.md`, `docs/` and the chart (re-grepped clean). The planning documents ship publicly by the author's decision. Remaining `Tailscale` mention is a technical comment about the 100.64.0.0/10 range in `internal/auth/oauth/clients.go` |
| Module path | `go.mod` | `github.com/team-memory/team-memory` matches the intended repository URL (confirmed by the author); no change needed |
| Untracked worktrees | `.claude/worktrees/` | present locally, untracked/ignored; not in history |

Unresolved before publishing: none from this audit. The security contact is set (GitHub private reporting, or the author's public email).
