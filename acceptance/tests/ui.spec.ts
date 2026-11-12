import { expect, Page, test } from "@playwright/test";
import { bearer, jsonParseFailure, openSettings, signIn, unique } from "./support";

// Browser flows on the settings page, run in Chromium and WebKit (Safari).
// The agent-key form posts multipart FormData through fetch: the Safari bug in
// crd_y1ZYQjwlAjkwsz64OGoJnqLm lived in that exact path.

async function createKey(page: Page, name: string, accountKey = false) {
  await page.getByLabel("Name this agent").fill(name);
  if (accountKey) await page.getByLabel(/Account key: can mint/).check();
  await page.getByRole("button", { name: "Create key" }).click();
}

const keyStatus = (page: Page) => page.locator("#agent-key-form-status");
const keyRow = (page: Page, name: string) => page.locator("[data-agent-key]").filter({ has: page.getByRole("heading", { name, exact: true }) });

test.beforeEach(async ({ page }) => {
  await signIn(page);
  await openSettings(page);
});

test("creates a normal agent key in place and the key works on the API", async ({ page, request }, info) => {
  const name = unique(info, "UI key");
  await page.evaluate(() => ((window as any).__noReload = true));
  await createKey(page, name);

  const token = page.getByRole("textbox", { name: "New agent API key" });
  await expect(token).toHaveValue(/^raises_agent_/);
  await expect(token).toBeInViewport();
  await expect(keyStatus(page)).toHaveText("");
  await expect(keyRow(page, name)).toBeVisible();
  await expect(keyRow(page, name).getByText("Account key", { exact: true })).toBeHidden();
  expect(await page.evaluate(() => (window as any).__noReload)).toBe(true);

  const value = await token.inputValue();
  const projects = await request.get("/v1/projects", bearer(value));
  expect(projects.status()).toBe(200);
  // A normal key cannot manage other keys.
  const mint = await request.post("/v1/agent-keys", { ...bearer(value), data: { name: "Nope" } });
  expect(mint.status()).toBe(403);
});

test("creates an account key that can mint keys on the API", async ({ page, request }, info) => {
  const name = unique(info, "UI account key");
  await createKey(page, name, true);

  const token = page.getByRole("textbox", { name: "New agent API key" });
  await expect(token).toHaveValue(/^raises_agent_/);
  await expect(keyStatus(page)).toHaveText("");
  await expect(keyRow(page, name).getByText("Account key", { exact: true })).toBeVisible();

  const mint = await request.post("/v1/agent-keys", { ...bearer(await token.inputValue()), data: { name: unique(info, "Minted from UI key") } });
  expect(mint.status(), await mint.text()).toBe(201);
  const minted = await mint.json();
  expect((await request.get("/v1/projects", bearer(minted.token))).status()).toBe(200);
});

test("bootstrap prompt appears in place, copies, and exchanges on the API", async ({ page, request }) => {
  if (test.info().project.name === "chromium") {
    await page.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  }
  await page.setViewportSize({ width: 1024, height: 600 });
  await page.evaluate(() => {
    (window as any).__noReload = true;
    window.scrollTo(0, 120);
  });
  const before = await page.evaluate(() => window.scrollY);
  expect(before).toBeGreaterThan(0);

  await page.getByRole("button", { name: "Create bootstrap prompt" }).click();
  const prompt = page.getByRole("textbox", { name: "Bootstrap prompt" });
  await expect(prompt).toHaveValue(/bootstrap token boot_/);
  await expect(prompt).toBeInViewport();
  await expect(page.locator("#bootstrap-form-status")).toHaveText("");

  const afterCreate = await page.evaluate(() => window.scrollY);
  await page.getByRole("button", { name: "Copy bootstrap prompt" }).click();
  await expect(page.locator("#copy-bootstrap-prompt-status")).toHaveText(/Copied to clipboard|Prompt selected/);
  // Neither creating nor copying reloads the page or jumps back to the top.
  expect(await page.evaluate(() => (window as any).__noReload)).toBe(true);
  expect(page.url()).toMatch(/\/settings$/);
  expect(afterCreate).toBeGreaterThan(0);
  expect(await page.evaluate(() => window.scrollY)).toBe(afterCreate);

  const text = await prompt.inputValue();
  if (test.info().project.name === "chromium") {
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(text);
  }

  const bootstrapToken = text.match(/boot_[A-Za-z0-9_-]+/)?.[0];
  expect(bootstrapToken).toBeTruthy();
  const exchange = await request.post("/v1/bootstrap/exchange", { data: { token: bootstrapToken, name: "Bootstrapped in browser" } });
  expect(exchange.status(), await exchange.text()).toBe(201);
  const { token } = await exchange.json();
  expect((await request.get("/v1/projects", bearer(token))).status()).toBe(200);
});

test("revoking a key in the UI makes the API reject it", async ({ page, request }, info) => {
  const name = unique(info, "UI revoke");
  await createKey(page, name);
  const token = await page.getByRole("textbox", { name: "New agent API key" }).inputValue();
  expect(token).toMatch(/^raises_agent_/);
  expect((await request.get("/v1/projects", bearer(token))).status()).toBe(200);

  await keyRow(page, name).getByRole("button", { name: "Revoke key" }).click();
  await expect(page).toHaveURL(/\/settings$/);
  await expect(keyRow(page, name).getByText("Revoked")).toBeVisible();
  await expect(keyRow(page, name).getByRole("button", { name: "Revoke key" })).toHaveCount(0);

  expect((await request.get("/v1/projects", bearer(token))).status()).toBe(401);
});

test.describe("key form shows the server's own errors", () => {
  test("an invalid name", async ({ page }) => {
    await createKey(page, "Bad<name>");
    await expect(keyStatus(page)).toContainText("agent name is not valid");
    await expect(keyStatus(page)).not.toHaveText(jsonParseFailure);
    await expect(page.locator("#new-agent-key")).toBeHidden();
  });

  test("a stale CSRF token", async ({ page }) => {
    await page.locator('#agent-key-form input[name="csrf"]').evaluate((input: HTMLInputElement) => (input.value = "stale"));
    await createKey(page, "Stale form");
    await expect(keyStatus(page)).toContainText("could not verify this browser session");
    await expect(keyStatus(page)).not.toHaveText(jsonParseFailure);
  });

  test("an expired session", async ({ page }) => {
    await page.context().clearCookies();
    await createKey(page, "Signed out");
    await expect(keyStatus(page)).toContainText("sign in again to create a key");
    await expect(keyStatus(page)).not.toHaveText(jsonParseFailure);
  });
});
