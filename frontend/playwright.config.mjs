import { defineConfig } from '@playwright/test';
import { PORT, STORAGE_STATE } from './browser/setup.mjs';

const baseURL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: './browser',
  testMatch: '*.spec.mjs',
  globalSetup: './browser/setup.mjs',
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 60000,
  use: { baseURL, storageState: STORAGE_STATE, trace: 'retain-on-failure', screenshot: 'only-on-failure' },
  projects: ['light', 'dark'].flatMap((colorScheme) => [390, 1280].map((width) => ({
    name: `${colorScheme}-${width}`,
    use: { browserName: 'chromium', colorScheme, viewport: { width, height: 900 }, locale: 'en-US', timezoneId: 'UTC' },
  }))),
  webServer: {
    command: 'node browser/server.mjs',
    url: baseURL,
    reuseExistingServer: false,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 10000 },
  },
});
