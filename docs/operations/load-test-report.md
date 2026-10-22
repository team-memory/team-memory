# Load test report (stage 06)

Date: 2026-10-22. Tool: `loadtest/` (Go, closed loop: each virtual user sends its next request when the previous one returns). Raw results: `deploy/dev-k3d/.local/load-results-*.json` (not committed; regenerate with the commands at the end).

## Summary against the targets

| Target (proposal) | Measured p95 at concurrency 1 / 8 / 32 | Verdict |
| --- | --- | --- |
| Search p95 < 500 ms, warm, **excluding query embedding** | **8 / 154 / 60 ms** | **Met** at every level |
| Write p95 < 300 ms, excluding async embedding | **71 / 87 / 261 ms** | **Met** in the reported run. At 32 it is close to the limit: an earlier run with the same configuration measured **550 ms** (see variance) |
| Search end to end, **including** query embedding (no target) | 1,443 / 2,226 / 2,127 ms | Far above 500 ms on this hardware: CPU-only Ollama. Not a target miss, but users will feel it |

Availability (99.9%) was not measured by this test; see the rollout drill in [upgrade.md](upgrade.md) for request loss during rollouts (zero for 0.6.0 restarts).

## Environment

| | |
| --- | --- |
| Host | Apple M4 (10 cores), 16 GB, macOS; Docker Desktop 29.9.2 VM with 10 CPUs and 7.7 GiB |
| Cluster | k3d 5.9.0 (k3s v1.36.4): 1 server, 1 agent node, both in the same VM; Traefik ingress with TLS; kube-router NetworkPolicy enforced |
| Team Memory | image `0.6.0-dev`, chart defaults: **2 API replicas, 250m CPU / 256 Mi each** (memory limit 256 Mi, no CPU limit), 1 worker 250m / 256 Mi. pgx pool default (max(4, CPUs) = 10 connections per pod) |
| PostgreSQL | 17 + pgvector 0.8.1, in-cluster StatefulSet, no resource limits, default settings, local-path volume |
| Embeddings | Ollama 0.13.0, `nomic-embed-text`, **CPU only** in a container (no Metal or GPU) |
| Tracing | 100% sampling to Jaeger (needed to separate the query-embedding time; adds overhead to every request) |
| Client | the load tool on the same Mac, through the ingress (`https://load.localtest.me`), so client, ingress, API, database and Ollama share 10 cores |
| Rate limits | **raised** for the run (`api.rateLimit`, `portal.rateLimit` = 100,000/s): they protect the service and would otherwise turn this into a rate-limit test. Portal users signed in before measuring (sign-in is limited per client address, and all virtual users share one) |

This is a laptop, not the target cluster. Treat absolute numbers as indicative; the shape (where time goes, what saturates first) is the useful part.

## Dataset

Synthetic engineering data (`loadtest seed`), **not pilot evidence**:

- One organization, a scope tree of **341 scopes, 5 levels** (organization → 4 teams → 16 groups → 64 subgroups → 256 projects).
- **1,001 principals, 4,000 grants**: each principal has a grant at a random depth (cascading readers above projects; writers and maintainers on projects) plus three non-cascading reader grants elsewhere, so RLS works over the hierarchy as in a real tree.
- Memories: 16,210 seeded (60 per project, 10 per inner scope) plus 16,622 written by an earlier aborted run = **32,832 when the search phase ran**; the write phase added 19,666, so the portal phases ran at about **52,500**.
- Embeddings: about 6,900 from the model (the worker, before it was stopped) and about 26,000 **synthetic**: every vocabulary word was embedded once by the real model and each document's vector is the normalized sum of its words' vectors. Queries are always embedded by the real model. Reason: CPU-only Ollama embedded about **0.6 documents/s** (16 documents per 25–30 s batch), so model-embedding the dataset would have taken over 15 hours. The HNSW index, similarity floor and iterative scan run exactly as in production; result relevance was not the subject.
- 64 load users with tokens: 16 writers or maintainers on projects, the rest readers at all depths (including the organization root, which sees the whole tree).

## Results

Latency in milliseconds. 45 s per cell. `n` successful requests, errors in brackets.

### MCP

| Scenario | c | n | rps | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `memory_search` end to end | 1 | 46 | 1.0 | 870 | 1,443 | 1,570 | 1,720 |
| | 8 | 171 | 3.8 | 2,053 | 2,226 | 2,334 | 2,445 |
| | 32 | 689 | 15.3 | 2,034 | 2,127 | 2,280 | 2,343 |
| `memory_search` excluding query embedding (from traces: tool span minus the Ollama call) | 1 | 46 | | 4.3 | **8.2** | 10.7 | |
| | 8 | 165 | | 28.5 | **154.1** | 208.5 | |
| | 32 | 629 | | 13.2 | **60.3** | 106.8 | |
| query embedding alone (Ollama) | 1 | | | 863 | 1,434 | | |
| | 8, 32 | | | 2,001 | 2,003–2,005 | | |
| `memory_create` | 1 | 1,342 | 29.8 | 30.2 | **71.0** | 82.8 | 153 |
| | 8 | 7,812 | 173.6 | 42.3 | **87.1** | 126.2 | 263 |
| | 32 | 10,512 | 233.6 | 124.8 | **261.3** | 370.5 | 991 |

### Portal and scope-tree navigation

| Page | c | n | rps | p50 | p95 | p99 | max |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Home with scope navigator | 1 | 6,159 | 136.9 | 3.9 | 26.1 | 36.2 | 98 |
| | 8 | 35,185 | 781.9 | 8.4 | 21.7 | 34.1 | 1,685 |
| | 32 | 50,109 | 1,113.5 | 24.8 | 63.4 | 96.8 | 256 |
| Scope page (own and inherited memories) at the user's depth | 1 | 2,825 | 62.8 | 10.4 | 50.1 | 63.3 | 122 |
| | 8 | 9,502 | 211.1 | 32.4 | 82.2 | 125.4 | 276 |
| | 32 | 12,165 | 270.3 | 105.9 | 250.8 | 326.6 | 657 |
| Memory page (history, diffs, conflicts) | 1 | 2,064 | 45.9 | 16.3 | 56.6 | 114.5 | 580 |
| | 8 | 9,652 | 214.5 | 33.4 | 75.2 | 110.1 | 274 |
| | 32 | 10,053 | 223.4 | 130.9 | 297.9 | 397.9 | 868 |
| Review queue (maintainers; others get their scope page) | 1 | 2,715 | 60.3 | 11.0 | 50.2 | 63.9 | 500 |
| | 8 | 7,617 | 169.3 | 30.5 | 97.6 | 486.0 | 1,673 |
| | 32 | 8,725 | 193.9 | 116.0 | 297.9 | 1,608.8 | 3,823 |
| Portal search (embeds the query too) | 1 | 21 | 0.5 | 2,070 | 2,199 | | 2,271 |
| | 8 | 165 | 3.7 | 2,102 | 2,314 | 2,383 | 2,541 |
| | 32 | 639 | 14.2 | 2,130 | 2,562 | 2,810 | 2,891 |

No request failed in any cell of the reported runs. The `portal_home` row at 32 is from a rerun after adding the sign-in warm-up: the first attempt had 2,970 sign-in refusals (HTTP 429 from the per-address sign-in limit), which the warm-up removed; its page latency was the same (p95 65 ms).

## Findings

1. **The database path meets the search target with a wide margin**, including RLS over a 5-level tree with 4,000 grants and readers at the root who see all 341 scopes. Most of the work is a handful of millisecond queries (a 20-span trace showed about 13 ms of database time).
2. **Query embedding dominates search latency** on CPU-only Ollama: about 0.9 s at concurrency 1, and from concurrency 8 every call hits the 2 s `TEAM_MEMORY_QUERY_EMBED_TIMEOUT`, after which search falls back to full text and still answers (no errors). Throughput is capped near 15 searches/s by Ollama, not by Team Memory. Run the model on a GPU or Apple silicon natively, or lower the timeout if full-text results are acceptable sooner.
3. **Writes** stay under 300 ms up to 8 concurrent writers with room to spare and approach the limit at 32 (about 230 writes/s, more than a team will produce). The cost is the write transaction: RLS checks, revision, event, outbox rows, audit and the full-text column.
4. **Portal pages** stay under 300 ms p95 at 32 concurrent users. The review queue has a long tail at 8 and 32 (p99 0.5–1.6 s): it runs `ConflictsFor` for up to 25 items per view. That is the first page to optimize if review queues grow.
5. **Embedding backfill** is the slowest operation in the system on CPU: about 0.6 documents/s. Plan backfills (new model, restore without embeddings) accordingly: [deploy.md](deploy.md).

## Variance and caveats

- An earlier run with the same configuration (before the portal limits were raised; 16,210 memories at the start, so a smaller dataset) measured write p95 of 66 / 160 / **550 ms** and search-excluding-embedding p95 of 11 / 19 / 35 ms. Search was consistent across runs; writes at concurrency 32 were not. On this shared laptop, concurrency 32 is at the edge of capacity, so the write target is **not reliably met at 32** here. Re-measure on the target cluster.
- Search excluding embedding at concurrency 8 (154 ms) was higher than at 32 (60 ms) in the reported run, and 19 ms in the earlier run. The measurement subtracts the Ollama span from the tool span; it includes the full-text fallback and queueing for database connections, which varied between runs. All values are well under target.
- 100% trace sampling adds overhead to every request (one span per SQL statement). Production uses 10%.
- The load generator shares the CPUs it measures.

## Reproduce

```sh
make k3d-up                                                        # disposable cluster
# deploy a solo release with the values in deploy/dev-k3d (values-load.yaml shows the overrides used)
DATABASE_URL=<operator URL of the release database> go run ./loadtest seed --users users.json
DATABASE_URL=... go run ./loadtest embed-synthetic                 # only with a slow embedding model
go run ./loadtest run --base https://load.localtest.me --users users.json --ca deploy/dev-k3d/.local/ca.pem \
  --duration 45s --concurrency 1,8,32 --jaeger http://localhost:16686 --out results.json
```
