import { defineConfig, devices } from '@playwright/test'

// Starts a fresh Intrabin binary (built with `make build`) on a temporary data directory.
const port = Number(process.env.E2E_PORT ?? 18080)
const runnerPort = Number(process.env.E2E_RUNNER_PORT ?? 18090)
const runnerToken = 'e2e-runner-token-0123456789abcdef'
const dnsMockPort = Number(process.env.E2E_DNSMOCK_PORT ?? 18096)
const dataDir = process.env.E2E_DATA_DIR ?? `/tmp/intrabin-e2e-${Date.now()}`

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'], viewport: { width: 1366, height: 900 } } },
  ],
  webServer: [{
    // Fake Cloudflare/ArvanCloud APIs (tests/dnsmock); real providers are never contacted.
    command: '../../bin/dnsmock',
    url: `http://127.0.0.1:${dnsMockPort}/healthz`,
    reuseExistingServer: false,
    timeout: 30_000,
    env: { DNSMOCK_LISTEN: `127.0.0.1:${dnsMockPort}`, DNSMOCK_CF_TOKEN: 'e2e-cf-token', DNSMOCK_ARVAN_KEY: 'e2e-arvan-key' },
  }, {
    // The isolated Action Runner used by workflow script steps.
    command: `rm -rf ${dataDir}-runner && ../../bin/intrabin runner`,
    url: `http://127.0.0.1:${runnerPort}/v1/health`,
    reuseExistingServer: false,
    timeout: 30_000,
    env: {
      INTRABIN_RUNNER_LISTEN: `127.0.0.1:${runnerPort}`,
      INTRABIN_RUNNER_TOKEN: runnerToken,
      INTRABIN_RUNNER_WORKDIR: `${dataDir}-runner`,
      INTRABIN_LOG_LEVEL: 'warn',
    },
  }, {
    command: `rm -rf ${dataDir} && ../../bin/intrabin`,
    url: `http://127.0.0.1:${port}/api/v1/health`,
    reuseExistingServer: false,
    timeout: 30_000,
    env: {
      INTRABIN_LISTEN: `127.0.0.1:${port}`,
      INTRABIN_DATA_DIR: dataDir,
      INTRABIN_SETUP_TOKEN: 'e2e-setup-token-0123456789',
      INTRABIN_MIN_INTERVAL: '10s',
      INTRABIN_LOG_LEVEL: 'warn',
      INTRABIN_CLUSTER_LISTEN: `127.0.0.1:${process.env.E2E_CLUSTER_PORT ?? 17999}`,
      INTRABIN_RUNNER_URL: `http://127.0.0.1:${runnerPort}`,
      INTRABIN_RUNNER_TOKEN: runnerToken,
      INTRABIN_DNS_TEST_CLOUDFLARE_API: `http://127.0.0.1:${dnsMockPort}/cloudflare/client/v4`,
      INTRABIN_DNS_TEST_ARVANCLOUD_API: `http://127.0.0.1:${dnsMockPort}/arvan/cdn/4.0`,
    },
  }],
})
