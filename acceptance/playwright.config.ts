import { defineConfig, devices } from "@playwright/test";

// The suite runs the real Raises server and SQLite store through the
// cmd/raises-acceptance harness. Only GitHub OAuth and outbound webhook sends
// are replaced, so nothing leaves the machine. See acceptance/README.md.
const port = Number(process.env.ACCEPTANCE_PORT || 18080);
const baseURL = `http://127.0.0.1:${port}`;
const binary = process.env.RAISES_ACCEPTANCE_BIN || ".bin/raises-acceptance";
const build = process.env.RAISES_ACCEPTANCE_BIN
  ? ""
  : `(cd .. && CGO_ENABLED=0 go build -o acceptance/.bin/raises-acceptance ./cmd/raises-acceptance) && `;

export default defineConfig({
  testDir: "tests",
  // One server and one database for the whole run: keep it serial so the
  // seeded state stays deterministic.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  timeout: 30_000,
  expect: { timeout: 5_000 },
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  webServer: {
    command: `${build}ACCEPTANCE_ADDR=127.0.0.1:${port} ${binary}`,
    url: `${baseURL}/readyz`,
    reuseExistingServer: false,
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
  projects: [
    { name: "api", testMatch: /api\.spec\.ts/ },
    { name: "chromium", testMatch: /ui\.spec\.ts/, use: { ...devices["Desktop Chrome"] } },
    { name: "webkit", testMatch: /ui\.spec\.ts/, use: { ...devices["Desktop Safari"] } },
  ],
});
