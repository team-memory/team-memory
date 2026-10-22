# Authentication deployment guide

This guide deploys Team Memory on Kubernetes with the Helm chart in `deploy/helm/team-memory` and sets up sign-in. Follow the steps in order. Every credential is created as a Kubernetes Secret **before** anyone signs in; no secret value ever goes into `values.yaml`.

Two modes:

| Mode | Who it is for | Sign-in |
| --- | --- | --- |
| **Solo** | one person, or a small group without an IdP | personal access tokens (PATs) for MCP clients and the portal |
| **Team** | anyone with an OpenID Connect IdP (Keycloak, Google, Entra ID, Okta, Dex, ...; SAML through a broker) | MCP clients use OAuth through Team Memory; the portal uses the same IdP |

In both modes, the **first owner is created with the CLI** (`team-memory init`). Nobody becomes owner by signing in first, and an email-domain match never grants ownership. In team mode the owner then links their SSO identity explicitly.

Placeholders used below: namespace `team-memory`, release `team-memory`, public URL `https://memory.example.com`, IdP `https://sso.example.com/realms/example`. Replace them with your own.

Tools: `kubectl`, `helm` 3.14+, `jq`, `openssl`.

## 1. Prerequisites

- **PostgreSQL 17 with pgvector 0.8** reachable from the cluster, and an empty database plus an owner role that can create roles (`CREATEROLE`) or is a superuser. See [deploy.md](deploy.md) for the external-versus-in-cluster choice.
- **An ingress controller** (the chart defaults to Traefik) and a TLS certificate for the public hostname, in a Secret named by `ingress.tls.secretName` (default `team-memory-tls`). Team Memory requires https in production: the issuer, cookies (`Secure`, `__Secure-` prefix) and the portal's Origin check all derive from `publicURL`.
- One hostname serves everything: `/mcp`, `/oauth/...` (including the IdP callback), `/.well-known/...` and the portal (default `/portal`). A separate portal hostname is not supported in this version.

```sh
kubectl create namespace team-memory
```

## 2. Database Secret and login roles

Team Memory connects with four PostgreSQL roles (see `docs/decisions/rls.md`): the **operator** (migrations and the CLI; owns the tables) and three runtime logins that are subject to row-level security: **app** (`serve`), **worker** (embeddings) and **mirror** (git export).

Create the Secret with the operator URL first. Put connection parameters such as `sslmode=require` in every URL that crosses a network.

```sh
kubectl -n team-memory create secret generic team-memory-db \
  --from-literal=operator-url='postgres://teammemory_owner:OWNER_PASSWORD@db.example.internal:5432/teammemory?sslmode=require'
```

Create the runtime logins with the operator CLI. It migrates the empty database first (creating the group roles), then prints a generated password **once**:

```sh
export TM_IMAGE=ghcr.io/team-memory/team-memory:0.6.0 TM_DB_SECRET=team-memory-db
CHART=deploy/helm/team-memory
$CHART/scripts/tm-admin.sh team-memory team-memory db create-login --role app --user tm_app
$CHART/scripts/tm-admin.sh team-memory team-memory db create-login --role worker --user tm_worker
$CHART/scripts/tm-admin.sh team-memory team-memory db create-login --role mirror --user tm_mirror   # only with the git mirror
```

Add the three URLs to the Secret (same host and database, each login's password):

```sh
kubectl -n team-memory patch secret team-memory-db --type merge -p "$(jq -n \
  --arg app    'postgres://tm_app:APP_PASSWORD@db.example.internal:5432/teammemory?sslmode=require' \
  --arg worker 'postgres://tm_worker:WORKER_PASSWORD@db.example.internal:5432/teammemory?sslmode=require' \
  --arg mirror 'postgres://tm_mirror:MIRROR_PASSWORD@db.example.internal:5432/teammemory?sslmode=require' \
  '{stringData: {"app-url": $app, "worker-url": $worker, "mirror-url": $mirror}}')"
```

`serve`, `worker` and `mirror worker` refuse to start with a role that is a superuser, has `BYPASSRLS`, owns tables or belongs to another component's group.

## 3. Team mode only: signing keys and IdP Secrets

Skip this section for solo mode.

### 3.1 Token-signing key

Team Memory signs its own access tokens. Create one EC P-256 key; the file name (without `.pem`) is the key id:

```sh
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out tm-2026-10.pem
kubectl -n team-memory create secret generic team-memory-signing --from-file=tm-2026-10.pem
shred -u tm-2026-10.pem 2>/dev/null || rm -P tm-2026-10.pem
```

### 3.2 Register Team Memory at the IdP

Every provider gets an **id** you choose (lowercase, e.g. `keycloak`, `google`, `entra`). Register this **redirect URI** exactly, with no trailing slash:

```
https://memory.example.com/oauth/callback/<id>
```

The same callback serves MCP client sign-in and portal sign-in. Team Memory is a **confidential client** using the authorization code flow with PKCE, scopes `openid email profile`. Provider recipes are in section 8.

### 3.3 IdP client secret

One Secret per provider, key `client-secret`:

```sh
read -rs -p 'client secret: ' CS; echo
kubectl -n team-memory create secret generic team-memory-idp-keycloak --from-literal=client-secret="$CS"
unset CS
```

The chart mounts each provider Secret as a file, without `subPath`, and Team Memory reads it on every use, so rotation needs no restart (section 9).

## 4. Values

Solo mode, `values-solo.yaml`:

```yaml
publicURL: https://memory.example.com
database:
  existingSecret: team-memory-db
ingress:
  host: memory.example.com
  tls: {secretName: team-memory-tls}
# The ingress controller's pod network, so rate limits and audit see real client addresses.
trustedProxies: [10.42.0.0/16]
networkPolicy:
  database:
    to: [{ipBlock: {cidr: 10.0.0.5/32}}]   # where PostgreSQL is
```

Team mode, `values-team.yaml`, adds:

```yaml
sso:
  enabled: true
  signingKeys:
    existingSecret: team-memory-signing
  providers:
    - id: keycloak
      displayName: Company SSO
      issuer: https://sso.example.com/realms/example
      clientID: team-memory
      existingSecret: team-memory-idp-keycloak
      allowedEmailDomains: [example.com]
portal:
  # First deployment only: lets the owner use their token to Connect SSO (step 7).
  tokenSignIn: owner
```

Portal settings (`portal.*`) map to `TEAM_MEMORY_PORTAL_*`: `basePath` (default `/portal`; `/` serves it at the root of the host), `tokenSignIn` (`auto`: on in solo mode, off with SSO; `owner`; `all`; `off`), `idleTimeout` (30m), `absoluteTimeout` (12h), `sourceSchemes`, `cspExtra`. There is **no session secret to manage**: sessions and CSRF tokens are random values stored hashed in PostgreSQL. Cookies are always `HttpOnly` and `SameSite=Lax`, and `Secure` with the `__Secure-` prefix when `publicURL` is https.

### Using an external secret operator

Every Secret above is referenced by name only, so it can be produced by External Secrets Operator, Sealed Secrets or the Secrets Store CSI driver instead of `kubectl create secret`, as long as it has the documented keys. Example with External Secrets Operator:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: team-memory-idp-keycloak, namespace: team-memory}
spec:
  secretStoreRef: {name: vault, kind: ClusterSecretStore}
  target: {name: team-memory-idp-keycloak}
  data:
    - secretKey: client-secret
      remoteRef: {key: team-memory/keycloak, property: client-secret}
```

## 5. Deploy

```sh
helm upgrade --install team-memory deploy/helm/team-memory -n team-memory -f values-team.yaml \
  --set image.tag=0.6.0
```

The pre-install hook runs `team-memory migrate` (already applied in step 2; it is idempotent).

## 6. Check readiness

```sh
kubectl -n team-memory rollout status deploy/team-memory-api --timeout=3m
kubectl -n team-memory get pods
```

The API pods become **Ready** only when the database is reachable and, in team mode, every provider's discovery succeeded and its client secret is readable. `/readyz` is not exposed through the ingress; to read it:

```sh
kubectl -n team-memory port-forward svc/team-memory 18080:80 &
curl -s localhost:18080/readyz; kill %1
```

`ok` means ready. Anything else is explained in section 10.

From outside, check discovery through the ingress (team mode):

```sh
curl -fsS https://memory.example.com/.well-known/oauth-protected-resource
```

## 7. Establish the owner (once)

```sh
deploy/helm/team-memory/scripts/tm-admin.sh team-memory team-memory \
  init --org acme --org-name "Acme" --owner "Ada Lovelace"
```

It prints the organization, the default project and a **personal access token, once**. Store it in a password manager. Rerunning `init` never replaces an existing owner.

**Solo mode**: you are done with authentication.

- Portal: open `https://memory.example.com/portal`, choose *Sign in with a token*, paste the token.
- MCP clients: configure `https://memory.example.com/mcp` with header `Authorization: Bearer <token>` (stage 07 documents each client).
- More people: `tm-admin.sh ... principal create --name "Grace"`, `member add --scope acme/default --principal <id> --role writer`, `token create --principal <id>`.

**Team mode**: link the owner's SSO identity.

1. Open `https://memory.example.com/portal`, sign in with the owner token (allowed because `portal.tokenSignIn: owner`).
2. *Account* → **Connect SSO** → choose the provider and sign in there. Team Memory requires proof of both identities: the portal session and a fresh IdP login. It refuses an identity already linked to someone else and never links by email alone.
3. Sign out and sign in again with SSO to confirm.
4. Close token sign-in: set `portal.tokenSignIn: auto` (or remove the line) and run `helm upgrade` again.

## 8. Onboard users (team mode)

Users sign in to the portal with SSO, or let their MCP client start the OAuth flow. The first login of an unknown identity creates a principal **with no roles** (unless the provider has `autoProvision: false`, which refuses unknown identities). Grant roles as the owner:

```sh
tm-admin.sh team-memory team-memory principal list
tm-admin.sh team-memory team-memory member add --scope acme/default --principal <id> --role writer
```

Or map IdP groups to roles and set `syncGroupRoles: true` on the provider, so roles follow group membership at every sign-in:

```sh
tm-admin.sh team-memory team-memory idp-groups add --issuer https://sso.example.com/realms/example \
  --group platform-team --scope acme/platform --role writer
```

## 9. Provider recipes

### Keycloak (self-hosted)

1. Realm → *Clients* → *Create client*: type **OpenID Connect**, client id `team-memory`.
2. *Capability config*: **Client authentication on** (confidential), **Standard flow** on, Direct access grants off, Implicit off, OAuth 2.0 Device off. PKCE method S256 is used by Team Memory.
3. *Login settings*: Valid redirect URIs `https://memory.example.com/oauth/callback/keycloak`; Web origins empty (no browser calls Keycloak's token endpoint).
4. *Credentials* tab: copy the client secret into the Secret of step 3.3.
5. Users need a verified email for `allowedEmailDomains` (*Users* → *Email verified*), or drop the domain list and rely on realm membership.
6. Groups (optional): *Client scopes* → `team-memory-dedicated` → *Add mapper* → *Group Membership*, token claim name `groups`, **Full group path off**, add to ID token on. Then use `allowedGroups` and/or `syncGroupRoles`.
7. Issuer: `https://<keycloak-host>/realms/<realm>`.

Keycloak's own `prompt=login` and `max_age=0` support is what Connect SSO relies on for a fresh login; Keycloak includes `auth_time`.

### Google (hosted)

1. Google Cloud console → *Google Auth Platform* → *Clients* → *Create client*: type **Web application**.
2. Authorized redirect URI: `https://memory.example.com/oauth/callback/google` (must match exactly, including scheme and no trailing slash).
3. Copy the client secret into `team-memory-idp-google`.
4. Provider values:

   ```yaml
   - id: google
     displayName: Google
     issuer: https://accounts.google.com
     clientID: 1234567890-abc.apps.googleusercontent.com
     existingSecret: team-memory-idp-google
     allowedEmailDomains: [example.com]
   ```

   Google sends `email_verified` with the `email` scope. For a Google Workspace domain, also set the OAuth consent screen to **Internal**, so only accounts of your organization can sign in (Google documents the `hd` claim as the reliable organization signal; Team Memory checks the verified email's domain).
5. Google issues no group claim; manage roles with `member add`.

### Microsoft Entra ID (hosted)

1. Entra admin center → *App registrations* → *New registration*: **single tenant** ("Accounts in this organizational directory only").
2. Redirect URI: platform **Web**, `https://memory.example.com/oauth/callback/entra`.
3. *Certificates & secrets* → new client secret; copy its **Value** into `team-memory-idp-entra`. Note its expiry and rotate before it (section 10).
4. Issuer is tenant-specific: `https://login.microsoftonline.com/<tenant-id>/v2.0`.
5. Restrict who may sign in in Entra itself: *Enterprise applications* → the app → *Properties* → **Assignment required = Yes**, then assign users or groups.
6. **Do not use `allowedEmailDomains` with Entra.** Entra ID tokens carry no `email_verified` claim, and Microsoft documents the `email` claim as not guaranteed to be correct, so a domain allowlist would refuse everyone. Use assignment, and optionally groups:
7. Groups (optional): *Token configuration* → *Add groups claim* → *Security groups*, ID token: **Group ID**. Entra sends group **object ids**; use those in `allowedGroups` and `idp-groups add --group <object-id>`. Users in more than 200 groups receive an overage marker instead of the list; assign the app to the relevant groups only, or do not rely on groups for them.

   ```yaml
   - id: entra
     displayName: Microsoft
     issuer: https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0
     clientID: 11111111-1111-1111-1111-111111111111
     existingSecret: team-memory-idp-entra
     groupsClaim: groups
   ```

### SAML IdPs (through a broker)

Team Memory speaks OpenID Connect only. For a SAML-only IdP (ADFS, Shibboleth, older Okta or OneLogin setups), put an OIDC broker in front:

- **Keycloak**: create a realm, add the SAML IdP under *Identity providers* → *SAML v2.0* (import its metadata), map the SAML NameID or a stable attribute to the username and the email attribute to `email` (mark it verified with a mapper if your IdP guarantees it), and optionally set *First login flow* to auto-link. Then register Team Memory in that realm exactly as in the Keycloak recipe.
- **Dex**: use the `saml` connector and register Team Memory as a Dex static client. Dex does not support dynamic client registration, which Team Memory does not need upstream.

The `sub` that Team Memory stores is the broker's subject, so keep the broker's user identity stable (don't recreate the realm).

## 10. Secret rotation without downtime

| Secret | Procedure |
| --- | --- |
| IdP client secret | Create the new secret at the IdP while the old one is still valid (Keycloak: *Client secret rotation* policy, or regenerate in a quiet minute; Google and Entra allow two secrets). Update the Kubernetes Secret (`kubectl create secret ... --dry-run=client -o yaml \| kubectl apply -f -`). The kubelet refreshes the file within about a minute and Team Memory reads it on the next login. Sign in once to confirm, then revoke the old secret at the IdP. |
| Token-signing key | Keys are loaded at startup; rotation is two rolling restarts, never a gap. (1) Add the new `<kid>.pem` to `team-memory-signing` and `kubectl rollout restart deploy/team-memory-api`: both keys are published, the old one still signs. (2) Set `sso.signingKeys.activeSigningKey: <new kid>` and `helm upgrade`. (3) After the access-token lifetime (15 minutes by default), remove the old key from the Secret and restart again. |
| Database passwords | See [rotate-secrets.md](rotate-secrets.md) (create a second login, switch, drop the first). |
| Mirror credential | See [mirror.md](mirror.md#repository-access-administration). |

## 11. Troubleshooting a server that is not ready

Read `/readyz` (section 6) and the API log (`kubectl -n team-memory logs deploy/team-memory-api`). With SSO configured, a server that cannot use it **stays not ready and refuses every MCP, OAuth and portal request with 503**. It never falls back to tokens or to no authentication.

| `/readyz` says | Cause | Fix |
| --- | --- | --- |
| `not ready: database unavailable` | PostgreSQL unreachable or credentials wrong | Check the `app-url` in the Secret, `networkPolicy.database.to`, and the database. Pods that start with a bad role log `unsafe database role` and exit. |
| `not ready: sso: provider "keycloak" client secret: open /etc/team-memory/idp/keycloak/client-secret: no such file or directory` | The provider's Secret, or its `client-secret` key, is missing | Create the Secret with key `client-secret` (step 3.3). The pod becomes ready within about a minute, without a restart. |
| `not ready: sso: provider "keycloak" client secret file ... is empty` | The key exists but is empty | Put the secret value in it. |
| `not ready: sso: provider "keycloak": discovery ...` | The issuer URL is wrong or unreachable (DNS, TLS, egress policy) | Check `issuer` (must match the IdP's `iss` exactly), `networkPolicy.idp`, and a private CA (`extraVolumes` plus `SSL_CERT_FILE`). Discovery is retried in the background. |
| `not ready: sso: sso config ...` / `signing key ...` | `sso.json` rejected, or no readable signing key | Fix the values; check `team-memory-signing` exists and holds a `.pem` file. Unknown settings are rejected, not ignored. |
| `not ready: shutting down` | The pod received SIGTERM | Normal during rollouts. |

Other symptoms:

- **IdP shows "invalid redirect_uri"**: the registered URI differs from `<publicURL>/oauth/callback/<id>` (scheme, host, trailing slash).
- **Login works but the portal says the email domain is not allowed**: the IdP did not send `email_verified: true`, or the domain is not listed. With Entra, remove `allowedEmailDomains`.
- **Portal POSTs fail with 403 after sign-in**: `publicURL` does not equal the browser's origin (scheme, host and port must match exactly).
- **Everyone is rate limited together**: set `trustedProxies` to the ingress controller's pod network.
- **Connect SSO says the login was not fresh**: the IdP ignored `prompt=login`/`max_age=0` or sent no `auth_time`. Sign out of the IdP and retry; with Dex, see `docs/decisions/identity-provider.md`.

## Evidence

This guide was followed literally on a disposable k3d cluster (Traefik ingress, in-cluster PostgreSQL, Keycloak 26 as the upstream IdP): see [docs/handoff/06-operations.md](../handoff/06-operations.md) for the solo and SSO runs and the missing-Secret readiness check.
