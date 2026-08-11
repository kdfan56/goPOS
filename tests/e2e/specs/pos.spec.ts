import { test, expect } from '@playwright/test';

test.use({ httpCredentials: { username: 'testadmin', password: 'testpass' } });

// Known seed product from seedIfEmpty
const SEED_BARCODE = '8964000123456';
const SEED_PRODUCT_NAME = "Olper's Milk 1L";
const SEED_PRICE = 290;

test.describe('POS Terminal', () => {

  test.beforeEach(async ({ page }) => {
    await page.goto('/pos');
  });

  test('page loads with barcode input and disabled pay buttons', async ({ page }) => {
    await expect(page.locator('#barcode')).toBeVisible();
    await expect(page.locator('#barcode')).toBeFocused();
    await expect(page.locator('#pay-cash')).toBeDisabled();
    await expect(page.locator('#pay-card')).toBeDisabled();
    await expect(page.locator('#grand-total')).toHaveText('0');
    await expect(page.locator('#item-count')).toHaveText('0');
  });

  test('scanning a known barcode adds product to cart', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');

    await expect(page.locator('#msg')).toHaveText(/Added: /);
    await expect(page.locator('.cart')).toBeVisible();
    await expect(page.locator('.cart tbody tr')).toHaveCount(1);
    await expect(page.locator('.cart tbody tr td:first-child')).toHaveText(SEED_PRODUCT_NAME);
  });

  test('scanning same barcode twice increments quantity', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    await expect(page.locator('.cart tbody tr')).toHaveCount(1);
    await expect(page.locator('.qty')).toHaveText('2');
    await expect(page.locator('#grand-total')).toHaveText(String(SEED_PRICE * 2));
  });

  test('increment and decrement buttons work', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    await page.locator('button[data-action="inc"]').click();
    await expect(page.locator('.qty')).toHaveText('2');

    await page.locator('button[data-action="dec"]').click();
    await expect(page.locator('.qty')).toHaveText('1');
  });

  test('void button removes item from cart', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    await page.locator('button[data-action="void"]').click();

    await expect(page.locator('.empty-cart')).toBeVisible();
    await expect(page.locator('#pay-cash')).toBeDisabled();
    await expect(page.locator('#pay-card')).toBeDisabled();
  });

  test('checkout with cash completes and shows receipt', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    const [receiptPage] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#pay-cash').click(),
    ]);

    await receiptPage.waitForLoadState();
    // Receipt h1 is "goPOS"; check the title and meta div instead
    await expect(receiptPage).toHaveTitle(/Receipt/);
    await expect(receiptPage.locator('.meta')).toContainText(/Receipt/);
    await expect(receiptPage.locator('body')).toContainText(SEED_PRODUCT_NAME);

    // Main page should show success and reset
    await expect(page.locator('#msg')).toHaveText(/Sale #.*complete/);
    await expect(page.locator('#grand-total')).toHaveText('0');
    await receiptPage.close();
  });

  test('checkout with card completes and shows receipt', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    const [receiptPage] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#pay-card').click(),
    ]);

    await receiptPage.waitForLoadState();
    await expect(receiptPage).toHaveTitle(/Receipt/);
    await expect(receiptPage.locator('.meta')).toContainText(/Receipt/);
    await receiptPage.close();
  });

  test('checkout with multiple items shows correct total', async ({ page }) => {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    await page.locator('#barcode').fill('8964000234567');
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    // Inc the first item (+ row, data-index="0") to make qty 2
    await page.locator('button[data-action="inc"][data-index="0"]').click();
    await page.waitForTimeout(200);

    // Total: 2 × 290 (Milk) + 1 × 180 (Bread) = 760
    await expect(page.locator('#grand-total')).toHaveText('760');
    await expect(page.locator('#item-count')).toHaveText('3');
  });

  test('scanning unknown barcode shows error', async ({ page }) => {
    await page.locator('#barcode').fill('0000000000000');
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(500);

    await expect(page.locator('#msg')).toHaveText(/not found/);
    await expect(page.locator('#msg')).toHaveClass(/error/);
  });

  // Station identity (decision 25). The label is unique per run, so that
  // parallel workers cannot land on the same till and break the count.
  test('a sale records the till that rang it up', async ({ page }) => {
    const till = 'T' + Date.now().toString().slice(-8);

    await page.goto('/pos?station=' + till);
    await expect(page.locator('#gpStation')).toHaveText('Till ' + till);

    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(300);

    const [receiptPage] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#pay-cash').click(),
    ]);
    await receiptPage.close();

    // The till reaches the dashboard as its own row, with exactly this sale.
    await page.goto('/');
    const row = page.locator('.panel', { hasText: "Today's Sale — by till" })
                    .locator('tbody tr', { hasText: till });
    await expect(row).toHaveCount(1);
    await expect(row.locator('td').nth(1)).toHaveText('1');
    await expect(row.locator('td').nth(2)).toHaveText(String(SEED_PRICE));
  });

  // The station survives a reload without the query parameter, because a till
  // is set up once with a bookmark and then used all day.
  test('the till is remembered after the query parameter is gone', async ({ page }) => {
    const till = 'R' + Date.now().toString().slice(-8);

    await page.goto('/pos?station=' + till);
    await expect(page.locator('#gpStation')).toHaveText('Till ' + till);

    await page.goto('/pos');
    await expect(page.locator('#gpStation')).toHaveText('Till ' + till);

    // An empty value clears it, so a till can be un-assigned without devtools.
    await page.goto('/pos?station=');
    await expect(page.locator('#gpStation')).toHaveText('Till not set');
  });
});
