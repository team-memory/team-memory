# Repository audit (stage 07)

Date: 2026-10-22. Scope: working tree and all 18 commits on `main`.

| Check | Method | Result |
| --- | --- | --- |
| Secrets in tree and history | `make secret-scan` (gitleaks, tree + git history) | no leaks found |
| Known vulnerabilities | `govulncheck ./...` | 0 affecting the code. GO-2026-5932 (`golang.org/x/crypto/openpgp`, unmaintained, no fix) is in a required module; the code does not call it. Reported by CI as informational |
| Dependency licenses | `go-licenses csv ./...` | 29 Apache-2.0, 21 BSD-3, 2 BSD-2, 19 MIT, 1 Unlicense. `segmentio/asm` reports "Unknown" but its LICENSE is MIT No Attribution. `cyphar/filepath-securejoin` is BSD-3 with MPL-2.0 file-level terms; compatible when used unmodified. No GPL/AGPL. |
| Private hostnames / IPs | grep for RFC1918 ranges and hostnames in tracked files | none; placeholders `memory.example.com`, `*.localtest.me` only |
| Personal data | grep | commit author name and email (`tyler@lafronz.com`) are in git history; normal for authorship, **author to confirm** they want that email public |
| Internal-only references | grep | `prompts/` and `proposal.md` contain local absolute paths (`/Users/.../team-memory/`), and mention the author's MicroK8s/Tailscale setup. Not secrets. **Author decides** whether to ship `prompts/`, `review*.md`, `proposal.md` publicly or trim them |
| Module path | `go.mod` | `github.com/team-memory/team-memory` is a placeholder org; must be changed to the real GitHub path before publishing (every import changes) |
| Untracked worktrees | `.claude/worktrees/` | present locally, untracked/ignored; not in history |

Unresolved before publishing: the three **author decides** items and the SECURITY.md contact. None is a secret or private-host finding.
