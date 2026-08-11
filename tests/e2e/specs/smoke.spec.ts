import { test, expect } from '@playwright/test';

test.use({ httpCredentials: { username: 'testadmin', password: 'testpass' } });

test.describe('Smoke tests — key pages load correctly', () => {

  test('dashboard shows 5 summary tiles', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('.tile')).toHaveCount(5);
    await expect(page.locator('.tile .label')).toContainText(['Today\'s Sale', 'Transactions', 'Avg Basket', 'Low Stock']);
  });

  // The parity dashboard (decision 24). The count alone would pass after a
  // rename, so every panel is also pinned by its heading. Adding a panel is
  // meant to fail here: update both lists in the same edit.
  test('dashboard shows every parity panel', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('.panel')).toHaveCount(9);
    for (const heading of [
      'Monthly sale',
      'No. of customers',
      "Today's Sale — by till",
      "Today's Refunds",
      'Payment method',
      'Top selling items',
      'Category contribution',
      'Sale vs purchase',
      'Reorder level',
    ]) {
      await expect(page.locator('.panel-header h2', { hasText: heading })).toHaveCount(1);
    }
  });

  // The charts are inline SVG built in Go (decision 24), so there is no chart
  // library to trust and the geometry is ours to get wrong. These two tests tie
  // each chart to a number that is already proven elsewhere on the same page,
  // so a broken query or a broken layout cannot pass quietly.
  test.describe('dashboard charts', () => {
    // A sale of our own guarantees both charts have data, whatever order the
    // parallel workers ran in. Without it, an empty chart renders a message
    // instead of an <svg> and the assertions below would be a race.
    test.beforeEach(async ({ page }) => {
      await page.goto('/pos');
      await page.locator('#barcode').fill('8964000123456');
      await page.locator('#barcode').press('Enter');
      await page.waitForTimeout(300);
      const [receipt] = await Promise.all([
        page.waitForEvent('popup', { timeout: 10000 }),
        page.locator('#pay-cash').click(),
      ]);
      await receipt.close();
    });

    test('the customer chart agrees with the transactions tile', async ({ page }) => {
      await page.goto('/');

      // Both numbers are read from ONE page load, so another worker's sale
      // landing between two reads cannot make them disagree.
      const tile = await page.locator('.tile', { hasText: "Today's Transactions" })
                             .locator('.value').textContent();
      const newest = await page.locator('.panel', { hasText: 'No. of customers' })
                               .locator('rect.bar.new title').textContent();

      // The tile counts sale rows only (decision 21). The newest bar is today.
      expect(newest).toContain(tile!.trim() + ' sales');
    });

    test('the monthly chart has one bar per month and the newest is this month', async ({ page }) => {
      await page.goto('/');
      const chart = page.locator('.panel', { hasText: 'Monthly sale' });
      await expect(chart.locator('rect.bar')).toHaveCount(12);

      // The expected month comes from the month tile, not from the test
      // machine's clock. The server works in Asia/Karachi and the test runner
      // may not, so a JS Date would flake for a few hours every month end.
      const tileLabel = await page.locator('.tile .label', { hasText: /\d{4} Sale$/ }).textContent();
      const thisMonth = tileLabel!.trim().replace(/ Sale$/, '');

      const newest = await chart.locator('rect.bar.new title').textContent();
      expect(newest).toContain(thisMonth);
    });
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

  // These three assert the empty state, so they pin a date range that no other spec can
  // write into. Asserting "no sales exist" against the live range makes the test depend on
  // whether the POS and returns specs have run yet, which is a race between workers.
  const EMPTY_RANGE = '?from=2020-01-01&to=2020-01-02';

  test('categories report page loads', async ({ page }) => {
    await page.goto('/reports/categories' + EMPTY_RANGE);
    await expect(page.locator('h1')).toContainText(/Sales by Category/);
    await expect(page.locator('input[name="from"]')).toBeVisible();
    await expect(page.locator('input[name="to"]')).toBeVisible();
    await expect(page.locator('.empty')).toBeVisible();
  });

  test('itemwise margin report page loads', async ({ page }) => {
    await page.goto('/reports/itemwise' + EMPTY_RANGE);
    await expect(page.locator('h1')).toContainText(/Itemwise Margin/);
    await expect(page.locator('input[name="from"]')).toBeVisible();
    await expect(page.locator('input[name="to"]')).toBeVisible();
    await expect(page.locator('.empty')).toBeVisible();
  });

  test('purchases report page loads', async ({ page }) => {
    await page.goto('/reports/purchases' + EMPTY_RANGE);
    await expect(page.locator('h1')).toContainText(/Purchases/);
    await expect(page.locator('input[name="from"]')).toBeVisible();
    await expect(page.locator('input[name="to"]')).toBeVisible();
    // The current-price caveat must always be on the page, not only when rows exist
    await expect(page.locator('.caveat')).toBeVisible();
    await expect(page.locator('.empty')).toBeVisible();
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
