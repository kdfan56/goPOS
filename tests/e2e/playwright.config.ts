import { defineConfig } from '@playwright/test';

const TEST_ADMIN_USER = 'testadmin';
const TEST_ADMIN_PASS = 'testpass';
const TEST_CASHIER_USER = 'testcashier';
const TEST_CASHIER_PASS = 'testcashpass';

export { TEST_ADMIN_USER, TEST_ADMIN_PASS, TEST_CASHIER_USER, TEST_CASHIER_PASS };

export default defineConfig({
  testDir: './specs',
  timeout: 30000,
  retries: 1,
  use: {
    baseURL: 'https://localhost:8443',
    ignoreHTTPSErrors: true,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  globalSetup: './global-setup.ts',
  globalTeardown: './global-teardown.ts',
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
});
