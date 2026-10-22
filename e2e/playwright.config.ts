import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  workers: 1,
  fullyParallel: false,
  reporter: [['list']],
  use: { baseURL: process.env.PORTAL_URL, trace: 'off', screenshot: 'off' },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
});
