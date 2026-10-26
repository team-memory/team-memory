import { test, expect } from '@playwright/test';
import { mkdir } from 'node:fs/promises';
import path from 'node:path';

// Run only through the Go launcher, which supplies a disposable database,
// synthetic identities, and short-lived tokens. Never capture the filled login.
test('capture the documented reader and maintainer test drive', async ({ page }) => {
  const output = path.resolve(__dirname, '../../docs/assets/screenshots');
  await mkdir(output, { recursive: true });
  const capture = async (name: string) => {
    await page.screenshot({ path: path.join(output, `${name}.png`), fullPage: true, animations: 'disabled' });
  };
  const signIn = async (token: string) => {
    await page.goto('/portal/login');
    await page.getByLabel('Personal access token').fill(token);
    await page.getByRole('button', { name: 'Sign in with token' }).click();
    await expect(page).toHaveURL(/\/portal\/$/);
  };
  const scope = 'acme/platform/payments';
  const title = 'Use idempotency keys for payment retries';

  await page.goto('/welcome');
  await expect(page.getByRole('link', { name: /portal/i }).first()).toBeVisible();
  await capture('welcome');
  await page.goto('/portal/login');
  await expect(page.getByLabel('Personal access token')).toHaveValue('');
  await capture('sign-in');

  await signIn(process.env.TOKEN_READER!);
  await page.getByRole('link', { name: 'payments', exact: true }).click();
  await expect(page.getByRole('link', { name: title, exact: true })).toBeVisible();
  await expect(page.locator('article .trust-unverified')).toBeVisible();
  await capture('project-memories');

  await page.getByRole('tab', { name: /Inherited from ancestors/ }).click();
  await expect(page.getByText('Inherited · read-only', { exact: true })).toHaveCount(2);
  await expect(page.getByRole('link', { name: 'Keep credentials out of source control' })).toBeVisible();
  await capture('inherited-memories');

  await page.getByRole('searchbox', { name: 'Search memories', exact: true }).fill('payment');
  await page.getByRole('button', { name: 'Search', exact: true }).first().click();
  await expect(page.getByText(/Retrieval: fulltext_only/)).toBeVisible();
  await expect(page.getByRole('link', { name: title, exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Retry transient failures with backoff', exact: true })).toBeVisible();
  await capture('search');

  await signIn(process.env.TOKEN_MAINT!);
  await page.goto(`/portal/review/${scope}`);
  await expect(page.getByRole('heading', { name: 'Proposals awaiting review' })).toBeVisible();
  await capture('review-queue');
  await page.getByRole('link', { name: title, exact: true }).click();
  await expect(page.getByText('You are reviewing revision 2 exactly as shown above.')).toBeVisible();
  await page.getByLabel('Note (optional)').fill('Checked retry behavior, including process restarts.');
  // Capture the actual review panel without the long promotion/history sections.
  const review = page.locator('.review');
  await review.scrollIntoViewIfNeeded();
  const box = await review.boundingBox();
  const approve = await page.getByRole('button', { name: 'Approve revision 2', exact: true }).boundingBox();
  if (!box || !approve) throw new Error('review controls have no layout');
  await page.screenshot({ path: path.join(output, 'review.png'), animations: 'disabled',
    clip: { x: box.x, y: box.y, width: box.width, height: approve.y + approve.height - box.y + 10 } });
  await page.getByRole('button', { name: 'Approve revision 2', exact: true }).click();
  await expect(page.getByText('Approved. This exact revision is now trusted.')).toBeVisible();
  await expect(page.getByText(/Approved by Payments maintainer/)).toBeVisible();
  // A reader sees provenance and history without maintainer controls.
  await signIn(process.env.TOKEN_READER!);
  await page.goto(`/portal/m/${process.env.DEMO_MEMORY_ID}`);
  await page.locator('details.rev').filter({ hasText: 'Revision 2' }).locator('summary').first().click();
  await expect(page.getByLabel('Content changes')).toContainText('Persist the key');
  await capture('approved-history');

  await signIn(process.env.TOKEN_MAINT!);
  await page.goto(`/portal/audit/${scope}`);
  await page.getByLabel('Action', { exact: true }).fill('memory.approve');
  await page.getByRole('button', { name: 'Filter', exact: true }).click();
  await expect(page.locator('article')).toHaveCount(1);
  await expect(page.locator('article')).toContainText('Payments maintainer');
  await capture('audit-log');
});
