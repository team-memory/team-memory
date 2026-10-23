# Local Docker testing

`compose.local.yaml` runs the service and PostgreSQL 17 + pgvector without a host Go toolchain. It uses a separate Compose project (`team-memory-local`) and data volume from the database-only `compose.yaml`. PostgreSQL is not published to the host; the app and operations listener bind to loopback. Ports 8080 and 9090 must be free.

On each machine, clone this repository and run these commands from its root. Docker Desktop (macOS/Windows) or Docker Engine with Compose (Linux) must be running. The container build downloads its Go toolchain and dependencies; a first build needs internet access. Codex CLI is needed only for the client connection steps, and Python 3 for the token extraction snippet. Each machine initializes its own database and token; do not copy or commit another developer's credentials.

## First start

```bash
docker compose -f compose.local.yaml build app
docker compose -f compose.local.yaml up -d --wait postgres
umask 077
mkdir -p tmp/docker-local
docker compose -f compose.local.yaml run --rm operator init \
  --org local --owner 'Local Developer' > tmp/docker-local/bootstrap.txt
docker compose -f compose.local.yaml run --rm \
  -e TEAM_MEMORY_NEW_LOGIN_PASSWORD=tm-app-dev \
  operator db create-login --role app --user tm_app
docker compose -f compose.local.yaml up -d --wait app
```

`init` applies migrations and creates `local/default`, its owner, and an expiring personal access token. The bootstrap output contains the token and is saved only under the git-ignored `tmp/` directory with private permissions. The app connects using the restricted `tm_app` role; the operator database credential is not passed to the app container. Passwords in this Compose file are throwaway local defaults.

Initialization and role creation are **one-time steps per volume**. Do not rerun them on every start or overwrite the saved bootstrap output. To change the organization/project, also change `TEAM_MEMORY_DEFAULT_SCOPE` in the Compose file.

## Connect Codex

Create a private environment file from the saved bootstrap output (Python 3):

```bash
python3 - <<'PY'
from pathlib import Path
text = Path('tmp/docker-local/bootstrap.txt').read_text()
token = text.split('Personal access token (shown once, expires ')[1].split('\n\n')[1].strip()
p = Path('tmp/docker-local/token.env')
p.touch(mode=0o600)
p.chmod(0o600)
p.write_text('export TEAM_MEMORY_LOCAL_TOKEN=' + token + '\n')
PY
source tmp/docker-local/token.env
codex mcp add team_memory_local --url http://127.0.0.1:8080/mcp \
  --bearer-token-env-var TEAM_MEMORY_LOCAL_TOKEN
codex mcp get team_memory_local
codex
```

Launch Codex from that shell so it inherits the token. A running desktop process may need to be fully restarted with the variable in its environment; exporting in a terminal does not change an already-running app's environment. Restart/reconnect the MCP client after adding the server. The current conversation does not automatically gain a newly registered server.

Ask: “Use team_memory_local to create a fact titled Docker connection test in local/default, then search for it and fetch it by ID.” Memories start unverified. See [Codex client notes](../clients/codex.md) and [official MCP configuration documentation](https://developers.openai.com/codex/mcp).

## Check and manage the stack

- Portal: <http://localhost:8080/portal/> (sign in using the token from the bootstrap file).
- MCP: <http://127.0.0.1:8080/mcp> (bearer authentication required).
- Health/readiness: <http://127.0.0.1:9090/healthz> and <http://127.0.0.1:9090/readyz>.

```bash
curl --fail http://127.0.0.1:9090/readyz
docker compose -f compose.local.yaml ps
docker compose -f compose.local.yaml logs --tail=100 app
# Stop and retain data:
docker compose -f compose.local.yaml down
# Resume an initialized stack:
docker compose -f compose.local.yaml up -d --wait postgres app
```

On this already-initialized machine, resume with the `up` command above, then run `source tmp/docker-local/token.env` and launch `codex`. The Codex registration persists after shutdown, but requests fail while the service is stopped.

After code changes, build again, apply migrations with the operator, then recreate the app:

```bash
docker compose -f compose.local.yaml build app
docker compose -f compose.local.yaml run --rm operator migrate
docker compose -f compose.local.yaml up -d --wait app
```

To intentionally delete all local test data, run `docker compose -f compose.local.yaml down -v`, then repeat first start and regenerate the token environment file. Tokens from the deleted database will no longer work. To remove the Codex registration, run `codex mcp remove team_memory_local`.

This stack uses full-text search. Ollama, the embedding worker, SSO and the mirror are not configured. It is a local smoke-test environment, not the full cross-client release verification.

## Run the test suite

The container quickstart does not require host Go, but the repository test commands require Go 1.27+. Run from the repository root:

```bash
make test
```

This runs `go test -race -count=1 ./...` against the separate database-only Compose stack on `127.0.0.1:55432`. Integration tests create and drop their own databases. Use the Make target: a plain `go test ./...` without `TEST_DATABASE_URL` skips database tests.

For the browser tests, install Node.js/npm, then:

```bash
(cd e2e && npm ci && npx playwright install chromium)
make test-e2e
```

On Linux, Chromium may also need system dependencies (`npx playwright install --with-deps chromium` from `e2e/`). The browser test starts and seeds its own temporary server and verifies approval through MCP and the audit log; it does not use your local portal data.

`make test` skips opt-in Ollama retrieval evaluations, vector/mirror benchmarks, remote Git tests and the browser suite. Run the browser target separately as above. See the Makefile targets `eval`, `bench-vector` and `test-mirror-remote` for the extra services/configuration those checks require. Passing these suites does not complete the interactive cross-client release gate.

After testing, stop both project stacks while retaining their volumes:

```bash
docker compose -f compose.local.yaml down
docker compose -f compose.yaml down
```

Avoid `make db-down` if you want to retain the development database: that target deletes its volume.

## Verification recorded 2026-10-23

Built the pinned Dockerfile on Docker Desktop (Engine 29.9.2, linux/arm64), initialized a fresh volume, and started both containers healthy. HTTP checks from the Codex terminal confirmed:

- `/healthz`, `/readyz` and `/portal/login`: HTTP 200.
- Unauthenticated MCP POST: HTTP 401.
- Authenticated MCP initialization (protocol `2025-03-26`) and discovery of all eight tools.
- `memory_create` and `memory_search` succeeded in `local/default`; the created record remained unverified.

These checks used a direct HTTP smoke client, not an interactive Codex model tool call. The full stage-07 cross-client gate remains incomplete.

Repository validation on the host also passed: `make test` (all default unit/integration/security tests with the race detector) and `make test-e2e` (all four Chromium tests, including approval verified through MCP and the audit log). The opt-in evaluations, benchmarks and remote Git checks listed above were not run.
