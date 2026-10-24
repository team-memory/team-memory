import { test, expect, Page } from '@playwright/test';

// The Go launcher (integration/security/portal_e2e_test.go) seeds the data
// and passes tokens and ids through the environment.
const env = (k: string) => {
  const v = process.env[k];
  if (!v) throw new Error(`missing ${k}`);
  return v;
};

async function signIn(page: Page, token: string) {
  await page.goto('/portal/login');
  await page.getByLabel('Personal access token').fill(token);
  await page.getByRole('button', { name: 'Sign in with token' }).click();
  await expect(page).toHaveURL(/\/portal\/$/);
}

test('a project reader navigates down the tree, sees labeled inherited memories, searches and opens history', async ({ page }) => {
  await signIn(page, env('TOKEN_READER'));

  // The navigator lists only the reader's scope.
  await expect(page.getByRole('link', { name: 'payments', exact: true })).toBeVisible();
  await expect(page.getByText('billing')).toHaveCount(0);
  await page.getByRole('link', { name: 'payments', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'payments', level: 1 })).toBeVisible();
  await expect(page.getByText('decision for acme/platform/payments')).toBeVisible();
  await expect(page.getByText('decision for acme/platform/billing')).toHaveCount(0);

  // Inherited tab: labeled, read-only, grouped by origin.
  await page.getByRole('tab', { name: /Inherited from ancestors/ }).click();
  await expect(page.getByText('decision for acme', { exact: true })).toBeVisible();
  await expect(page.getByText('decision for acme/platform', { exact: true })).toBeVisible();
  await expect(page.getByText('Inherited · read-only').first()).toBeVisible();

  // Search uses the same service as MCP.
  await page.getByRole('searchbox', { name: 'Search memories' }).fill('zebracorn');
  await page.getByRole('button', { name: 'Search' }).first().click();
  await expect(page.getByText('decision for acme/platform/payments').first()).toBeVisible();
  await expect(page.getByText('billing')).toHaveCount(0);

  // History.
  await page.getByRole('link', { name: 'decision for acme/platform/payments' }).first().click();
  await expect(page.getByRole('heading', { name: 'History' })).toBeVisible();
  await page.getByText(/Revision 1/).click();
  await expect(page.getByRole('heading', { name: 'Lifecycle' })).toBeVisible();

  // A hidden memory looks like a missing one.
  const res = await page.goto('/portal/m/' + env('HIDDEN_ID'));
  expect(res?.status()).toBe(404);
  await expect(page.getByText('does not exist, or you do not have access')).toBeVisible();
});

test('the Content-Security-Policy is enforced and stored XSS stays inert', async ({ page }) => {
  await signIn(page, env('TOKEN_MAINT'));
  const dialogs: string[] = [];
  page.on('dialog', (d) => { dialogs.push(d.message()); void d.dismiss(); });
  const violations: string[] = [];
  await page.exposeFunction('reportViolation', (d: string) => violations.push(d));
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => (window as any).reportViolation(e.violatedDirective));
  });

  await page.goto('/portal/m/' + env('XSS_ID'));
  await expect(page.getByRole('heading', { level: 1 })).toContainText('<script>');
  expect(dialogs).toEqual([]);
  await expect(page.locator('main img')).toHaveCount(0);
  const link = page.getByRole('link', { name: 'click' });
  await expect(link).toHaveCount(0); // javascript: link is plain text

  // Inline script injected into the page is refused by the browser.
  const ran = await page.evaluate(() => {
    const s = document.createElement('script');
    s.textContent = 'window.__injected = true';
    document.head.appendChild(s);
    return (window as any).__injected === true;
  });
  expect(ran).toBe(false);
  await expect.poll(() => violations.length).toBeGreaterThan(0);
  expect(violations.join(' ')).toContain('script-src');

  // Framing is denied.
  const headers = (await page.request.get('/portal/login')).headers();
  expect(headers['x-frame-options']).toBe('DENY');
  expect(headers['content-security-policy']).toContain("default-src 'none'");
});

test('a maintainer approves an agent proposal for an exact revision', async ({ page }) => {
  await signIn(page, env('TOKEN_MAINT'));
  await page.goto('/portal/review/acme/platform/payments');
  await expect(page.getByRole('heading', { name: 'Proposals awaiting review' })).toBeVisible();
  await page.getByRole('link', { name: 'Use idempotency keys' }).click();
  await expect(page.getByText('not team policy')).toBeVisible();
  await expect(page.getByText('You are reviewing revision 1 exactly as shown above.')).toBeVisible();
  await page.getByLabel('Note (optional)').fill('matches our payments design');
  await page.getByRole('button', { name: 'Approve revision 1' }).click();
  await expect(page.getByText('Approved. This exact revision is now trusted.')).toBeVisible();
  await expect(page.getByText('Approved by')).toBeVisible();
});

test('phone width: no horizontal scroll, keyboard reaches the search box', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 700 });
  await signIn(page, env('TOKEN_READER'));
  await page.goto('/portal/s/acme/platform/payments');
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
  expect(overflow).toBe(false);
  await page.keyboard.press('Tab');
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused();
});


test('maintainers can filter audit events and readers cannot open the audit page', async ({ page }) => {
  await signIn(page, env('TOKEN_MAINT'));
  await page.goto('/portal/s/acme/platform/payments');
  await page.getByRole('link', { name: 'Audit log', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Audit log', exact: true })).toBeVisible();
  await page.getByLabel('Action', { exact: true }).fill('memory.create');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await expect(page.locator('article').first()).toContainText('memory.create');
  await page.getByLabel('Client name or OAuth ID').fill('no-such-client');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await expect(page.getByText('No matching audit events.')).toBeVisible();
  await signIn(page, env('TOKEN_READER'));
  const res = await page.goto('/portal/audit/acme/platform/payments');
  expect(res?.status()).toBe(403);
});
