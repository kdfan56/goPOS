import { test, expect } from '@playwright/test';
import { BrowserContext } from 'playwright';

const CASHIER = { username: 'testcashier', password: 'testcashpass' };
const ADMIN = { username: 'testadmin', password: 'testpass' };

/** Create a new browser context with the given HTTP Basic Auth credentials. */
async function authContext(browser: any, creds: { username: string; password: string }): Promise<BrowserContext> {
  return browser.newContext({
    httpCredentials: creds,
    ignoreHTTPSErrors: true,
  });
}

test.describe('Authentication and Role Access', () => {

  // ── No-auth tests (no credentials set) ──

  test('returns 401 when no credentials provided', async ({ browser }) => {
    const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
    const page = await ctx.newPage();
    const resp = await page.goto('/');
    expect(resp?.status()).toBe(401);
    await ctx.close();
  });

  test('returns 401 with bad credentials', async ({ browser }) => {
    const ctx = await browser.newContext({
      httpCredentials: { username: 'bad', password: 'guy' },
      ignoreHTTPSErrors: true,
    });
    const page = await ctx.newPage();
    const resp = await page.goto('/');
    expect(resp?.status()).toBe(401);
    await ctx.close();
  });

  // ── Admin access ──

  test.describe('admin access', () => {
    test.use({ httpCredentials: ADMIN });

    test('can access dashboard', async ({ page }) => {
      const resp = await page.goto('/');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('h1')).toHaveText('Dashboard');
      await expect(page.locator('.tile')).toHaveCount(4);
    });

    test('can access products page', async ({ page }) => {
      const resp = await page.goto('/products');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('h1')).toHaveText(/Products/);
    });

    test('can access reports page', async ({ page }) => {
      const resp = await page.goto('/reports');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('h1')).toHaveText(/Sales Report/);
    });

    test('can access receiving page', async ({ page }) => {
      const resp = await page.goto('/receiving');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('h1')).toHaveText(/Receiving/);
    });

    test('can access suppliers page', async ({ page }) => {
      const resp = await page.goto('/suppliers');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('h1')).toHaveText(/Suppliers/);
    });

    test('can access POS terminal', async ({ page }) => {
      const resp = await page.goto('/pos');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('#barcode')).toBeVisible();
    });
  });

  // ── Cashier access ──

  test.describe('cashier access', () => {

    test('is redirected from dashboard to /pos', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      await page.goto('/');
      // Cashier gets 303 → Playwright follows the redirect; check final URL
      await expect(page).toHaveURL('/pos');
      await ctx.close();
    });

    test('can access POS terminal', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      const resp = await page.goto('/pos');
      expect(resp?.status()).toBe(200);
      await expect(page.locator('#barcode')).toBeVisible();
      await ctx.close();
    });

    test('can access price-check page', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      await page.goto('/price-check');
      // price-check has no <h1>; check the manual-input field instead
      await expect(page.locator('#manual-input')).toBeVisible();
      await ctx.close();
    });

    test('cannot access products page (403)', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      const resp = await page.goto('/products');
      expect(resp?.status()).toBe(403);
      await ctx.close();
    });

    test('cannot access reports page (403)', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      const resp = await page.goto('/reports');
      expect(resp?.status()).toBe(403);
      await ctx.close();
    });

    test('cannot access purchases report (403)', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      const resp = await page.goto('/reports/purchases');
      expect(resp?.status()).toBe(403);
      await ctx.close();
    });

    test('cannot access suppliers page (403)', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      const resp = await page.goto('/suppliers');
      expect(resp?.status()).toBe(403);
      await ctx.close();
    });

    test('cannot access receiving page (403)', async ({ browser }) => {
      const ctx = await authContext(browser, CASHIER);
      const page = await ctx.newPage();
      const resp = await page.goto('/receiving');
      expect(resp?.status()).toBe(403);
      await ctx.close();
    });
  });
});
