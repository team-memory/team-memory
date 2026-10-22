# Decision: git library for the mirror

Status: accepted (stage 06, 2026-10-22)

## Decision

All git operations of the mirror use [go-git](https://github.com/go-git/go-git) **v5.19.3** (`github.com/go-git/go-git/v5`, with `go-billy/v5` v5.9.2), behind the small interface in `internal/mirror/git.go` (`Git`, `Repo`). The service never runs a `git` binary; the container image has none.

v5 is the current stable major version. v6 was at `v6.0.0-beta.1` on 2026-10-22 and is not used; moving to it means changing `internal/mirror/gogit.go` only.

## License check

go-git and go-billy are Apache-2.0, like Team Memory. Transitive dependencies added by go-git, checked from their LICENSE files in the module cache:

| Module | License |
| --- | --- |
| go-git, go-billy, golang/groupcache, pjbgf/sha1cd, skeema/knownhosts, xanzy/ssh-agent | Apache-2.0 |
| dario.cat/mergo, ProtonMail/go-crypto, cloudflare/circl, emirpasic/gods, go-git/gcfg, golang.org/x/crypto, golang.org/x/net, gopkg.in/warnings.v0 | BSD-style |
| Microsoft/go-winio, jbenet/go-context, kevinburke/ssh_config, sergi/go-diff | MIT |
| cyphar/filepath-securejoin | BSD-3-Clause and MPL-2.0 (file-level) |

All are compatible with distributing Team Memory under Apache-2.0. MPL-2.0 is file-level copyleft: its files must remain available in source form if modified, which they are not (they are used unmodified from the public module). Recheck when dependencies change (release checklist, stage 07).

## Enforcement

- `internal/mirror/noexec_test.go`, `TestNoProcessExecution`: no non-test package of the module imports `os/exec` or `golang.org/x/sys/execabs`.
- `TestFileTransportIsInProcess`: the `file://` transport is replaced (below).

## What was tested

| Path | Test | Result |
| --- | --- | --- |
| Local bare repository (`file://`): commit, push, second clone, kind change, sync discarding unpushed commits, non-fast-forward | `TestGoGitLocalBareRepository`, `TestGoGitRemoteDown` | pass |
| Authenticated **HTTPS** with a token, against a disposable Gitea 1.25.3 with a self-signed CA: push, second clone, server-backed non-fast-forward then sync and push, wrong token, untrusted certificate, token file rotated without restart, token never in error text | `TestMirrorHTTPSTokenRemote` (`deploy/dev-git/setup.sh`) | pass |
| Authenticated **SSH** with a deploy key: push, second clone, key limited to one repository, unregistered key, host-key mismatch (`known_hosts` pinned; RSA host key) | `TestMirrorSSHDeployKeyRemote` | pass |
| Full export pipeline against PostgreSQL (one commit per event, retries, acknowledgement loss, secret gate, verify, rebuild) | `integration/mirror_test.go` | pass |

Commits with a custom author (the acting principal and a no-reply address) and committer work as expected.

## Limits found and how the design works around them

1. **go-git v5's `file://` transport executes `git-upload-pack` and `git-receive-pack`.** `internal/mirror` replaces it at init with go-git's in-process server (`plumbing/transport/server`), so `file://` remotes work with no binary.
2. **The in-process server does not compare-and-swap reference updates** (it ignores the old value in the push command). It is only safe with a single writer, which the exporter guarantees: pushes are serialized per repository by a PostgreSQL advisory lock. Use `file://` for tests and local backups only; real servers (Gitea, GitHub, GitLab) enforce compare-and-swap, which the exporter relies on with several workers.
3. **The in-process server fails a fetch whose negotiation names an object it lacks** ("object not found"), which happened after an unpushed local commit. The working copy is a disposable cache, so `Sync` discards it and fetches once from scratch before reporting the remote as unavailable.
4. **go-git worktree operations scale with repository size.** `Worktree.Add` and status scan every file, which made a 1,000-event export take more than 20 s and a 5,000-event export about 9 minutes. The mirror uses a **bare** clone and writes blobs, trees and commits directly (`gogit.go`); no worktree exists.
5. **go-git's `Remote.Push` walks the whole remote history** (`revlist.ObjectsWithStorageForIgnores`) to compute which objects to send. At 5,000 commits, exporting **one** new event took 13 s. The mirror now sends exactly the objects it wrote since the last sync, in one packfile, through go-git's receive-pack transport, with the synced remote head as the expected old value. One event now takes 77 ms on a 5,000-commit repository.
6. **No delta compression is attempted** (pack window 0) and full trees are rewritten per commit, as git does. Packs and clones of large repositories are therefore bigger than `git` would make them: a cold clone of a 5,000-commit repository took 24 s. The cache is kept for the pod's lifetime (an `emptyDir`), so only a rescheduled pod pays this. Per-scope repositories keep histories short.
7. **Server-side non-fast-forward rejection** could not be forced deterministically: the exporter detects a moved remote before sending (the advertised head differs from the synced one) and returns `ErrNonFastForward`. A server's own rejection message (`non-fast-forward`, `fetch first`) is mapped to the same error. The Gitea test exercised the pre-send detection against a real server.
8. Shallow clones are not used: the idempotency check must see history back to the oldest unacknowledged event, and go-git's shallow support is limited.

## Measured export cost

`TestMirrorThroughput` (`TEAM_MEMORY_MIRROR_BENCH=N`), one repository, PostgreSQL 17 in Docker Desktop, Apple M4, `file://` remote:

| Memories (= commits) | Backfill of all events | One more event, warm cache | One more event, cold cache (new pod) |
| --- | --- | --- | --- |
| 1,000 | 6.1 s (164 events/s) | 36 ms | 1.06 s |
| 5,000 | 86.7 s (58 events/s) | 77 ms | 24.1 s |

Steady-state exports are fast; backfill and cold clones are one-time costs that grow with history. If one scope's history reaches tens of thousands of events, expect minutes for a cold clone; enable delta compression (`packfile.Encoder` window) or move to go-git v6 then.

## Alternatives considered

- **The `git` binary** (exec): rejected by the fixed project decision; it would also add a large package and its CVE surface to a distroless image.
- **libgit2 bindings (git2go)**: needs cgo and a C library in the image; rejected.
- **go-git v6 beta**: not stable yet.
