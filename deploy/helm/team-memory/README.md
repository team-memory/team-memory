# team-memory Helm chart

Deploys Team Memory: the API (`serve`: MCP endpoint, OAuth facade, web portal), the embedding worker, the optional git mirror worker, a migration hook, NetworkPolicies, a PodDisruptionBudget, an Ingress and optional Prometheus Operator resources. PostgreSQL is **not** part of the chart (see `docs/operations/deploy.md` for external versus in-cluster).

Start with `docs/operations/authentication.md`: it lists the Secrets to create **before** installing and the exact order of operations, for solo (personal access tokens) and team (upstream OIDC) mode.

```sh
helm upgrade --install team-memory deploy/helm/team-memory -n team-memory -f values.yaml
helm test team-memory -n team-memory
```

## Required values

| Value | Meaning |
| --- | --- |
| `publicURL` | exact browser origin, e.g. `https://memory.example.com` |
| `database.existingSecret` | Secret with `operator-url`, `app-url`, `worker-url`, `mirror-url` (key names configurable under `database.keys`) |
| `ingress.host` | the host of `publicURL` |
| `networkPolicy.database.to` | where PostgreSQL is (or `networkPolicy.enabled: false`) |

The chart refuses to render without them, with a message naming the value.

## Secrets the chart references (never creates)

| Secret | Keys | Used by |
| --- | --- | --- |
| `database.existingSecret` | `operator-url`, `app-url`, `worker-url`, `mirror-url` | migrate Job and verify CronJob (operator), api, worker, mirror |
| `sso.signingKeys.existingSecret` | `<kid>.pem` | api (team mode) |
| `sso.providers[].existingSecret` | `client-secret` | api (team mode), one per provider |
| `mirror.existingSecret` | `ssh-privatekey` + `known_hosts`, or `token` (+ `ca.crt` with `mirror.customCA`) | mirror worker, verify CronJob |
| `ingress.tls.secretName` | `tls.crt`, `tls.key` | ingress |

SSO Secrets are mounted `optional`, without `subPath`: a missing Secret leaves the API running but **not ready** with the reason in `/readyz`, and a rotated Secret is picked up without a restart. Any of these can come from an external secret operator.

## Components

| Component | Enabled when | Probes | Notes |
| --- | --- | --- | --- |
| api | always | `/readyz`, `/healthz` on 8080 | 2 replicas, rolling update with `maxUnavailable: 0`, PDB, topology spread, drains on SIGTERM |
| worker | `worker.enabled` and `embeddings.ollamaURL` set | ops port 9090 | |
| mirror | `mirror.enabled` | ops port 9090 | bare clones in an `emptyDir` |
| migrate Job | `migrations.enabled` | | `pre-install,pre-upgrade` hook, operator URL |
| verify CronJob | `mirror.verifyCronJob.enabled` | | `mirror verify`; a failed Job means drift |
| ServiceMonitor, PrometheusRule | `metrics.*.enabled` | | rules from `files/prometheus-rules.yaml` |

All pods: non-root (UID 65532), read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, no service account token.

## Values reference

See the comments in `values.yaml`. Groups: `image`, `publicURL`, `defaultScope`, `database`, `migrations`, `api`, `trustedProxies`, `embeddings`, `worker`, `portal`, `sso`, `mirror`, `tracing`, `service`, `ingress`, `metrics`, `networkPolicy`, `extraVolumes`/`extraVolumeMounts`, `serviceAccount`, security contexts.

## Operator commands

`scripts/tm-admin.sh <namespace> <release-fullname> <command...>` runs one `team-memory` operator command (`init`, `scope`, `member`, `token`, `jobs`, `mirror status|requeue|mark-purged`, ...) in a short-lived pod with the operator URL and prints its output. Before the first install, set `TM_IMAGE` and `TM_DB_SECRET`.

## Checks

`make helm-lint` (strict lint, solo and all-features values in `ci/`), `make check-alerts`. The chart was deployed to a disposable k3d cluster (`deploy/dev-k3d/`) in solo and SSO mode, with every component and NetworkPolicies enforced.
