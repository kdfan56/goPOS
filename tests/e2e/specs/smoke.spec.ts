import { test, expect } from '@playwright/test';

test.use({ httpCredentials: { username: 'testadmin', password: 'testpass' } });

test.describe('Smoke tests — key pages load correctly', () => {

  test('dashboard shows 4 summary tiles', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('.tile')).toHaveCount(4);
    await expect(page.locator('.tile .label')).toContainText(['Today\'s Sale', 'Transactions', 'Low Stock']);
    await expect(page.locator('.panel')).toHaveCount(2);
  });

  test('products page lists seeded items', async ({ page }) => {
    await page.goto('/products');
    await expect(page.locator('h1')).toContainText(/Products/);
    // 8 seeded products
    const rows = page.locator('table tbody tr');
    await expect(rows).toHaveCount(8);
    // Ordered by name alphabetically; "Coca-Cola 1.5L" is first
    await expect(rows.nth(0).locator('td').nth(1)).toHaveText("Coca-Cola 1.5L");
  });

  test('products page low stock filter shows empty state when nothing is low', async ({ page }) => {
    await page.goto('/products?low=1');
    // All 8 seeded products have stock > 10 threshold, so page shows empty state
    await expect(page.locator('h1')).toContainText(/Low stock/);
    await expect(page.locator('.empty')).toBeVisible();
  });

  test('new product form loads', async ({ page }) => {
    await page.goto('/products/new');
    await expect(page.locator('h1')).toContainText(/Add Product/);
    await expect(page.locator('input[name="barcode"]')).toBeVisible();
    await expect(page.locator('input[name="name"]')).toBeVisible();
    await expect(page.locator('input[name="price_rupees"]')).toBeVisible();
    await expect(page.locator('input[name="stock"]')).toBeVisible();
  });

  test('reports page loads with date range form and tables', async ({ page }) => {
    await page.goto('/reports');
    await expect(page.locator('h1')).toContainText(/Sales Report/);
    await expect(page.locator('input[name="from"]')).toBeVisible();
    await expect(page.locator('input[name="to"]')).toBeVisible();
    // There are 2 tables (daily sales + hour breakdown); check at least one
    await expect(page.locator('table').first()).toBeVisible();
  });

  test('receiving page loads', async ({ page }) => {
    await page.goto('/receiving');
    await expect(page.locator('h1')).toContainText(/Receiving/);
    await expect(page.locator('input[name="label"]')).toBeVisible();
  });

  test('suppliers page loads with empty state', async ({ page }) => {
    await page.goto('/suppliers');
    await expect(page.locator('h1')).toContainText(/Suppliers/);
    // No seeded suppliers, so there's no <table> — only an empty-state paragraph
    await expect(page.locator('.empty')).toBeVisible();
    await expect(page.locator('a[href="/suppliers/new"]')).toBeVisible();
  });

  test('price-check page loads', async ({ page }) => {
    await page.goto('/price-check');
    // price-check has no <h1>; confirm via the manual barcode input
    await expect(page.locator('#manual-input')).toBeVisible();
    await expect(page.locator('#manual-btn')).toBeVisible();
  });

  test('scan page loads', async ({ page }) => {
    await page.goto('/scan');
    // scan page has no <h1>; confirm via the page title and video container
    await expect(page).toHaveTitle(/Scan/);
    await expect(page.locator('#video')).toBeVisible();
  });
});
