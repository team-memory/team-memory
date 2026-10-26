import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './documentation',
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: process.env.PORTAL_URL,
    browserName: 'chromium',
    viewport: { width: 1440, height: 1000 },
    deviceScaleFactor: 1,
    colorScheme: 'light',
    locale: 'en-US',
    timezoneId: 'UTC',
    trace: 'off',
    video: 'off',
    screenshot: 'off',
  },
});
