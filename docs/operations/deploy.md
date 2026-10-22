# Deploy

Team Memory ships as one container image and one Helm chart (`deploy/helm/team-memory`, chart README there). Authentication setup, including the order in which Secrets are created, is in [authentication.md](authentication.md); this page covers the rest.

## Topology

```
            ingress (Traefik or any class), one hostname, TLS
                 │  /mcp  /oauth/*  /.well-known/*  /portal
                 ▼
   ┌──────── api (serve) ×2 ───────┐       ┌── worker ×1 ──┐   ┌── mirror ×1 (optional) ──┐
   │ MCP, OAuth facade, portal     │       │ embedding jobs │   │ git_mirror jobs          │
   │ app DB role (RLS)             │       │ worker DB role │   │ mirror DB role           │
   └───────┬──────────┬────────────┘       └──┬──────────┬──┘   └──┬───────────────┬───────┘
           │          └── IdPs (team mode)    │          │         │               └── git remote (SSH/HTTPS)
           │          └── Ollama (query)      │          └── Ollama (documents)
           ▼                                  ▼                    ▼
                    PostgreSQL 17 + pgvector (external by default)
                    └── pgBackRest: WAL archive + base backups → off-host repository
```

Every component also serves `/metrics`, `/healthz` and `/readyz` on port 9090 (never routed by the ingress). A pre-install/pre-upgrade Job runs `team-memory migrate` as the operator role.

## Sizing

Initial assumptions from the proposal, now checked against measurements ([load-test-report.md](load-test-report.md)):

| Component | Replicas | Requests | Limits | Notes |
| --- | --- | --- | --- | --- |
| api | 2 | 250m CPU, 256Mi | 256Mi memory, no CPU limit | PodDisruptionBudget `minAvailable: 1`; spread across nodes |
| worker | 1 | 250m, 256Mi | 256Mi | throughput is bound by the embedding model, not the worker |
| mirror | 1 | 100m, 128Mi | 512Mi | bare clones in an `emptyDir` (`mirror.cacheSizeLimit`) |
| PostgreSQL | 1 (+ standby if you need it) | 1–2 CPU, 2–4Gi | | dedicated storage; size for data + 2 days of WAL |

No CPU limits are set by default, to avoid CFS throttling on latency-sensitive requests; set them if your cluster requires.

**Embeddings**: CPU-only Ollama is the bottleneck for backfills. Measured in a Docker Desktop VM on an Apple M4: about 0.6 documents/s for `nomic-embed-text`, so a backfill of 16,000 memories takes hours. Run Ollama on a GPU (or natively on Apple silicon with Metal) for anything beyond a few thousand memories; new writes are embedded asynchronously and search falls back to full text meanwhile.

## PostgreSQL: external (default) or in-cluster

The chart never deploys PostgreSQL. Choose:

| | External (managed service or a VM) | In-cluster operator (e.g. CloudNativePG) |
| --- | --- | --- |
| Effort | Least: the provider or host handles storage, upgrades, backups | You operate the operator, its CRDs and upgrades |
| pgvector | Must be available (most managed PostgreSQL 17 offerings include it) | Use an image with pgvector (CloudNativePG publishes `postgresql` images with extensions, or build one) |
| Backups | pgBackRest on the host ([backup-restore.md](backup-restore.md)) or the provider's PITR | Built in (Barman Cloud to object storage, WAL archiving, PITR) |
| Failure domain | Separate from the cluster: a cluster rebuild does not touch data | Same cluster: storage-class and node failures matter; back up off-cluster |
| MicroK8s single node | Recommended (a small VM or the host itself) | Workable, but a node loss takes the database with it until restore |
| NetworkPolicy | `networkPolicy.database.to: [{ipBlock: {cidr: <db-ip>/32}}]` | a `namespaceSelector`/`podSelector` for the cluster's pods |

Whichever you choose, the database needs: PostgreSQL 17, pgvector 0.8, an owner role with `CREATEROLE` (or a DBA who pre-creates the group roles listed in `docs/decisions/rls.md`), TLS on the connection (`sslmode=require` or stronger), and `max_connections` of at least `(api replicas × pool_max_conns) + worker + mirror + operator + 10`. pgx defaults to `max(4, CPUs)` connections per pool; set `pool_max_conns` in the URL to control it.

## Image

- One image, `ghcr.io/team-memory/team-memory:<version>`, built from `Dockerfile`: multi-stage, static binary, distroless `static-debian12:nonroot` (UID 65532), no shell, no package manager, no git. About 43 MB.
- Base images are **pinned by digest** in the Dockerfile. To update: `docker buildx imagetools inspect <image>:<tag> --format '{{json .Manifest.Digest}}'`, replace the digest and the tag comment, rebuild, run the test suite.
- Deploy by digest in production: `--set image.digest=sha256:...`.
- `HEALTHCHECK` uses `team-memory healthcheck` (probes the operations listener).

## Install

1. Prerequisites and Secrets: [authentication.md](authentication.md) sections 1–3.
2. Values: start from `deploy/helm/team-memory/values.yaml`; the minimum is `publicURL`, `database.existingSecret`, `ingress.host` and `networkPolicy.database.to`. Set `trustedProxies` to the ingress controller's pod network.
3. Optional components: `embeddings.ollamaURL` (hybrid search and the worker), `mirror.*` ([mirror.md](mirror.md)), `tracing.otlpEndpoint`, `metrics.serviceMonitor`/`prometheusRule` (Prometheus Operator).
4. `helm upgrade --install team-memory deploy/helm/team-memory -n team-memory -f values.yaml`
5. `helm test team-memory -n team-memory` (checks API readiness through its Service).
6. `team-memory init` once (authentication.md section 7).
7. Backups: [backup-restore.md](backup-restore.md). Do not put real data in before WAL archiving works.

### MicroK8s notes

- Enable `dns`, `ingress` (or the `traefik` addon), `hostpath-storage` or another storage class if PostgreSQL runs in-cluster, and `metrics-server`.
- MicroK8s' NGINX ingress runs in namespace `ingress`; Traefik installed by Helm usually in `traefik`. Set `networkPolicy.ingressFrom` to that namespace, and `ingress.className` accordingly.
- MicroK8s uses Calico, which enforces NetworkPolicy. Its default pod CIDR is `10.1.0.0/16` (`trustedProxies`).
- For a Tailscale-only hostname, terminate TLS at the ingress with a certificate for that name (for example Tailscale's `tailscale cert`) and keep `publicURL` equal to it.

## NetworkPolicy behaviour to know

- Policies are default-deny per component; each dependency is allowed explicitly (`networkPolicy.*`). Hosted IdPs are allowed on 443 to non-private addresses; tighten if your IdP has fixed addresses.
- Some policy engines (k3s' kube-router was observed doing this) admit a **new** pod's traffic to a podSelector-protected peer one to three seconds after the pod starts. Long-lived components are unaffected; one-shot probes should retry (the chart's `helm test` uses `healthcheck --wait 60s`).
- Backups run next to PostgreSQL, so no Team Memory pod needs backup egress.

## Verified on

A disposable k3d cluster (k3s v1.36, Traefik, kube-router NetworkPolicy, PostgreSQL 17 + pgvector in-cluster, Keycloak 26, Ollama 0.13 CPU, Gitea 1.25 for the mirror, Jaeger and Prometheus): `deploy/dev-k3d/`. The author's MicroK8s cluster was **not** used; deploying there requires explicit confirmation and the values in the MicroK8s notes above.
