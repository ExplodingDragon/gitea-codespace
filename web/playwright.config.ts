import {defineConfig} from '@playwright/test';

export default defineConfig({
  testDir: './tests',
  workers: 1,
  use: {baseURL: 'http://127.0.0.1:18089', headless: true},
  webServer: {
    command: 'pnpm exec vite --host 127.0.0.1 --port 18089 --strictPort',
    url: 'http://127.0.0.1:18089',
    timeout: 120000,
  },
});
