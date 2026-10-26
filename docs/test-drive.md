# Team Memory test drive

This walkthrough follows one payment retry convention from an agent's MCP proposal to maintainer approval. Screenshots were captured from the running application in Chromium on 2026-10-26, using synthetic identities and data in a temporary PostgreSQL database.

The scope tree is `acme` (organization) → `platform` (team) → `payments` (project). A project reader browses and searches; a project maintainer reviews. The two ancestor memories are pre-approved fixtures. The project proposal is created and revised through real MCP calls by the Go SDK test client, whose reported name is `security-test` and whose synthetic principal is `CI agent`.

This run uses full-text search with no embedding provider. SSO, semantic retrieval, promotion, and external coding clients are outside this walkthrough. For client setup, see [Codex](clients/codex.md) and [Claude Code](clients/claude-code.md).

## 1. Find setup instructions and sign in

Open the service homepage at `/` or `/welcome`. It links to agent setup and the human portal. The unconfigured public hostname in this test server appears as `YOUR_MEMORY_HOST`; a deployment uses `TEAM_MEMORY_PUBLIC_URL` to supply its address.

![Public welcome page with agent connection instructions and links to the portal](assets/screenshots/welcome.png)

Open `/portal/` and sign in with a personal access token from the local bootstrap. The screenshot shows the empty form; no tokens are captured. An SSO deployment offers its configured provider instead; see [sign-in options](portal.md#signing-in).

![Portal sign-in page with an empty personal access token field](assets/screenshots/sign-in.png)

## 2. Browse the project's own memories

Choose **payments** from **Scopes**. The project has one active proposal, **Use idempotency keys for payment retries**. The agent first saved the basic retry rule, then added a second revision explaining how to persist the key across process restarts. Both calls leave the memory unverified.

The **Unverified** badge and warning make clear that this proposal has not become team policy. Scope filters let you narrow the list by kind, trust, status, and update date.

![Payments project listing its unverified idempotency-key convention with scope filters and an inherited memories tab](assets/screenshots/project-memories.png)

## 3. Read inherited guidance

Choose **Inherited from ancestors**. Organization guidance about credentials and platform guidance about retry backoff appear in separate groups, each labelled with its origin. Both are approved fixtures and read-only from this project. A project membership grants the relevant ancestor view without exposing sibling projects.

![Inherited tab grouping approved credential and retry guidance by organization and team origin](assets/screenshots/inherited-memories.png)

## 4. Search across the scope chain

Enter `payment` in **Search memories** and submit. The result list combines the local idempotency proposal with the inherited platform retry decision. It shows trust, author, origin scope, and whether a memory is inherited.

The selected mode is Hybrid, but **Retrieval: fulltext_only (disabled)** reports that this server has no embedding provider. The scores are ranks for this response, not confidence percentages. Portal search uses the same retrieval service as MCP.

![Search for payment returning an unverified project proposal and an approved inherited retry decision, with full-text retrieval status](assets/screenshots/search.png)

## 5. Review the exact revision

Sign in as the project maintainer and open **Review queue** from the scope page. The queue shows the latest revision of each unverified or cited proposal. Here, revision 2 is awaiting review.

![Maintainer review queue listing revision 2 of the idempotency-key proposal](assets/screenshots/review-queue.png)

Open the proposal, read its content and rationale, and check provenance. In **Maintainer review**, the page states **You are reviewing revision 2 exactly as shown above** and reports the secret scan. The maintainer adds a note and chooses **Approve revision 2**.

The promotion checkboxes remain unchecked. Approval therefore applies in `acme/platform/payments`; the convention is not shared up to the platform team. If the memory changes while a maintainer is reviewing it, the stale approval is refused.

![Cropped review panel showing the exact revision, secret-scan result, review note, optional promotion, and approval button](assets/screenshots/review.png)

## 6. Check approval and revision history

After approval, a reader reopens the memory. Its badge is now **Approved**, provenance names **Payments maintainer** as the reviewer of revision 2, and the creating and editing client is recorded. Expanding **Revision 2** shows the added instruction to persist the idempotency key; lifecycle events record creation, revision, and approval.

Approval covers this exact revision. A future edit would create another revision and reset approval, preserving the prior text and review in history.

![Approved convention with author and client provenance, revision 2's added-line diff, and lifecycle history](assets/screenshots/approved-history.png)

## 7. Check the audit trail

As the maintainer, open **Audit log** and filter **Action** to `memory.approve`. One matching event identifies the approving principal, outcome, portal entry point, and target memory. The expandable details contain audit and correlation information. Audit access is restricted to maintainers.

![Maintainer audit log filtered to memory.approve showing the successful browser approval](assets/screenshots/audit-log.png)

## Refreshing the screenshots

From the repository root, with Go, Docker, and Node.js/npm installed:

```bash
(cd e2e && npm ci && npx playwright install chromium)
make docs-screenshots
```

On Linux, install Chromium system dependencies if needed with `npx playwright install --with-deps chromium` from `e2e/`.

The target uses the database-only Compose stack on port 55432 and `TEST_DATABASE_URL` (the Makefile supplies the local default). It creates a fresh test database, starts a temporary HTTP server, seeds synthetic ancestor guidance, and creates and revises the project proposal through MCP. Playwright drives the portal at a 1440 × 1000 desktop viewport with light colors, English locale, and UTC time. The test checks search results, inheritance, the revision diff, browser approval, and the filtered audit event, then the Go launcher verifies the saved approval.

The capture sources are [`portal_documentation_test.go`](../integration/security/portal_documentation_test.go), [`documentation.config.ts`](../e2e/documentation.config.ts), and [`walkthrough.spec.ts`](../e2e/documentation/walkthrough.spec.ts). PNGs are written to [`docs/assets/screenshots/`](assets/screenshots/); the review image is cropped to its controls. Dates, generated IDs, and request IDs vary between runs. Captures contain synthetic content only; the login is captured before any token is entered. Traces, video, and automatic failure screenshots are disabled for this workflow.

The test closes its browser and HTTP servers and drops its temporary database. If you started the database-only Compose stack for this task, remove its containers and network afterwards while retaining its volume:

```bash
docker compose -f compose.yaml down
```

Leave a pre-existing development stack running. Visually inspect the updated PNGs before committing. To run the broader portal security and responsive browser checks separately, use `make test-e2e`.
