import { test, expect, Page } from '@playwright/test';

test.use({ httpCredentials: { username: 'testadmin', password: 'testpass' } });

const SEED_BARCODE = '8964000123456';
const SEED_PRODUCT_NAME = "Olper's Milk 1L";
const SEED_PRICE = 290;

/**
 * Ring up `qty` of the seed product and return the new sale's id.
 * The POS page reports it as "Sale #N complete" once checkout succeeds.
 */
async function makeSale(page: Page, qty: number): Promise<number> {
  await page.goto('/pos');
  for (let i = 0; i < qty; i++) {
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(250);
  }

  const [receiptPage] = await Promise.all([
    page.waitForEvent('popup', { timeout: 10000 }),
    page.locator('#pay-cash').click(),
  ]);
  await receiptPage.close();

  const msg = await page.locator('#msg').textContent();
  const match = msg?.match(/Sale #(\d+)/);
  expect(match, `could not read sale id from "${msg}"`).not.toBeNull();
  return parseInt(match![1], 10);
}

test.describe('Returns', () => {

  test('return page loads with the lookup form', async ({ page }) => {
    await page.goto('/pos/return');
    await expect(page.locator('h1')).toHaveText('Return');
    await expect(page.locator('#sale-id')).toBeVisible();
    await expect(page.locator('#lookup-btn')).toBeVisible();
    // The item table stays hidden until a sale is found
    await expect(page.locator('#sale-panel')).toBeHidden();
  });

  test('looking up a sale lists its returnable lines', async ({ page }) => {
    const saleId = await makeSale(page, 2);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();

    await expect(page.locator('#sale-panel')).toBeVisible();
    await expect(page.locator('#sale-meta')).toContainText(`Sale #${saleId}`);

    const row = page.locator('#lines tr').first();
    await expect(row.locator('td').nth(0)).toHaveText(SEED_PRODUCT_NAME);
    await expect(row.locator('td').nth(2)).toHaveText('2'); // sold
    await expect(row.locator('td').nth(3)).toHaveText('0'); // already returned
  });

  test('refunding one unit nets the money and opens a RETURN slip', async ({ page }) => {
    const saleId = await makeSale(page, 2);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();
    await expect(page.locator('#sale-panel')).toBeVisible();

    await page.locator('#lines .qty-input').first().fill('1');
    await expect(page.locator('#refund-total')).toHaveText(`Rs. ${SEED_PRICE}`);
    await expect(page.locator('#submit-btn')).toBeEnabled();

    const [slip] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#submit-btn').click(),
    ]);

    await slip.waitForLoadState();
    await expect(slip).toHaveTitle(/Return/);
    await expect(slip.locator('.return-mark')).toHaveText('RETURN');
    await expect(slip.locator('.meta')).toContainText(`against sale #${saleId}`);
    // Decision 19: the refund total is stored negative
    await expect(slip.locator('.grand')).toContainText(`-${SEED_PRICE}`);
    await slip.close();

    await expect(page.locator('#submit-status')).toContainText(`Refunded Rs. ${SEED_PRICE}`);

    // The page re-reads the sale, so the allowance must now show 1 already returned
    await expect(page.locator('#lines tr').first().locator('td').nth(3)).toHaveText('1');
  });

  test('a fully returned line cannot be returned again', async ({ page }) => {
    const saleId = await makeSale(page, 1);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();
    await expect(page.locator('#sale-panel')).toBeVisible();

    await page.locator('#lines .qty-input').first().fill('1');
    const [slip] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#submit-btn').click(),
    ]);
    await slip.close();

    // After the refresh the only line is exhausted: input disabled, submit disabled
    await expect(page.locator('#lines tr').first()).toHaveClass(/exhausted/);
    await expect(page.locator('#lines .qty-input').first()).toBeDisabled();
    await expect(page.locator('#submit-btn')).toBeDisabled();
  });

  test('the quantity box clamps to what is still returnable', async ({ page }) => {
    const saleId = await makeSale(page, 1);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();
    await expect(page.locator('#sale-panel')).toBeVisible();

    // Only 1 was sold; asking for 9 must fall back to 1
    await page.locator('#lines .qty-input').first().fill('9');
    await expect(page.locator('#lines .qty-input').first()).toHaveValue('1');
    await expect(page.locator('#refund-total')).toHaveText(`Rs. ${SEED_PRICE}`);
  });

  // Regression: posting the same product on two lines used to pass the over-return
  // guard, because each line was checked against the same untouched allowance.
  // A sale of 2 could be refunded as 4. The handler now folds by product first.
  test('the same product posted twice cannot exceed the allowance', async ({ page }) => {
    const saleId = await makeSale(page, 2);

    const resp = await page.request.post('/pos/return/submit', {
      data: {
        original_transaction_id: saleId,
        payment_method: 'cash',
        lines: [
          { product_id: 1, quantity: 2, sellable: true },
          { product_id: 1, quantity: 2, sellable: true },
        ],
      },
    });

    expect(resp.status()).toBe(400);
    const body = await resp.json();
    expect(body.error).toContain('cannot return 4');

    // And nothing was written: the sale is still fully returnable
    const check = await page.request.post('/pos/return/lookup', {
      data: { transaction_id: saleId },
    });
    const sale = await check.json();
    expect(sale.lines[0].qty_returned).toBe(0);
    expect(sale.lines[0].qty_returnable).toBe(2);
  });

  test('a split across two lines still totals correctly', async ({ page }) => {
    const saleId = await makeSale(page, 3);

    const resp = await page.request.post('/pos/return/submit', {
      data: {
        original_transaction_id: saleId,
        payment_method: 'cash',
        lines: [
          { product_id: 1, quantity: 2, sellable: true },
          { product_id: 1, quantity: 1, sellable: true },
        ],
      },
    });

    expect(resp.status()).toBe(200);
    const body = await resp.json();
    expect(body.refund_rupees).toBe(SEED_PRICE * 3);
  });

  test('the refund method defaults to how the customer paid', async ({ page }) => {
    // Ring up a card sale, then check the return screen preselects card.
    await page.goto('/pos');
    await page.locator('#barcode').fill(SEED_BARCODE);
    await page.locator('#barcode').press('Enter');
    await page.waitForTimeout(250);
    const [receiptPage] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#pay-card').click(),
    ]);
    await receiptPage.close();
    const saleId = parseInt((await page.locator('#msg').textContent())!.match(/Sale #(\d+)/)![1], 10);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();
    await expect(page.locator('#sale-panel')).toBeVisible();

    await expect(page.locator('#pay-method')).toHaveValue('card');
    await expect(page.locator('#pay-hint')).toContainText('card terminal');
  });

  test('looking up an unknown receipt shows an error', async ({ page }) => {
    await page.goto('/pos/return');
    await page.locator('#sale-id').fill('999999');
    await page.locator('#lookup-btn').click();

    await expect(page.locator('#lookup-status')).toContainText('no sale with that receipt number');
    await expect(page.locator('#sale-panel')).toBeHidden();
  });

  test('a return receipt cannot itself be returned', async ({ page }) => {
    const saleId = await makeSale(page, 1);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();
    await expect(page.locator('#sale-panel')).toBeVisible();

    await page.locator('#lines .qty-input').first().fill('1');
    const [slip] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#submit-btn').click(),
    ]);
    const returnId = parseInt(new URL(slip.url()).pathname.split('/').pop()!, 10);
    await slip.close();

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(returnId));
    await page.locator('#lookup-btn').click();

    await expect(page.locator('#lookup-status')).toContainText('is a return, not a sale');
  });

  /**
   * Decision 23: the avg basket tile must divide SALE money by the SALE count.
   * A refund normally settles an earlier day's receipt, so netting it against a
   * count of today's baskets divides two different sets of transactions.
   *
   * This test refunds part of a sale, so today always holds a return by the
   * time it reads the dashboard. That makes the two candidate numerators differ
   * by exactly the refund. The tile must sit ABOVE the net-money figure:
   *  - correct code (sale money)  -> avgBasket  >  netMoney / salesCount
   *  - the bug (net money)        -> avgBasket === netMoney / salesCount
   * The refund is deliberately large so the gap survives rounding even when
   * other specs have added many sales to the shared DB.
   *
   * Every number is read from ONE page load, so parallel workers cannot skew
   * the comparison and the test is safe to run in any order.
   */
  test('avg basket divides sale money by sale count, not net money', async ({ page }) => {
    const REFUND_QTY = 3;
    const saleId = await makeSale(page, 4);

    await page.goto('/pos/return');
    await page.locator('#sale-id').fill(String(saleId));
    await page.locator('#lookup-btn').click();
    await expect(page.locator('#sale-panel')).toBeVisible();
    await page.locator('#lines .qty-input').first().fill(String(REFUND_QTY));
    const [slip] = await Promise.all([
      page.waitForEvent('popup', { timeout: 10000 }),
      page.locator('#submit-btn').click(),
    ]);
    await slip.close();

    await page.goto('/');
    const tiles = page.locator('.tile');
    // Pin the labels: the reads below are positional, and a reordered tile
    // would otherwise compare the wrong numbers with a confusing message.
    await expect(tiles.nth(0).locator('.label')).toHaveText("Today's Sale");
    await expect(tiles.nth(1).locator('.label')).toHaveText("Today's Transactions");
    await expect(tiles.nth(2).locator('.label')).toHaveText('Avg Basket');
    // A return today is what makes the two numerators differ at all.
    await expect(tiles.nth(1).locator('.subline')).toContainText('return');

    const readNumber = async (i: number) => {
      const text = await tiles.nth(i).locator('.value').textContent();
      return parseInt(text!.replace(/[^0-9-]/g, ''), 10);
    };
    const netMoney = await readNumber(0);
    const salesCount = await readNumber(1);
    const avgBasket = await readNumber(2);

    expect(salesCount).toBeGreaterThan(0);
    expect(avgBasket).toBeGreaterThan(Math.round(netMoney / salesCount));
    // Sale money is never negative, so neither is the average.
    expect(avgBasket).toBeGreaterThan(0);
  });
});
