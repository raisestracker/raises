import { APIRequestContext, expect, Page, TestInfo } from "@playwright/test";

let counter = 0;

// unique returns a name no other test in this run uses. Both browser projects
// share one database, so names carry the project too.
export function unique(info: TestInfo, label: string): string {
  counter += 1;
  return `${label} ${info.project.name} ${counter}`;
}

export function slug(name: string): string {
  return name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
}

// Messages a browser produces when code calls response.json() on a body that
// is not JSON. The settings page must never show these to a person.
export const jsonParseFailure =
  /did not match the expected pattern|Unexpected token|is not valid JSON|JSON\.parse|unexpected character/i;

export type BrowserSession = { cookie: string; csrf: string };

// browserSession gets a signed-in session for a seeded account without a
// browser. The UI suite signs in through the real GitHub redirect flow instead.
export async function browserSession(request: APIRequestContext, user: "owner" | "other" = "owner"): Promise<BrowserSession> {
  const response = await request.get(`/__acceptance/sign-in${user === "other" ? "?user=other" : ""}`);
  expect(response.status()).toBe(200);
  return response.json();
}

// settingsCreateKey posts the settings key form exactly as the page's fetch
// client does: multipart FormData, Accept: application/json.
export async function settingsCreateKey(request: APIRequestContext, session: BrowserSession, name: string, accountKey = false) {
  const multipart: Record<string, string> = { csrf: session.csrf, name };
  if (accountKey) multipart.account_key = "1";
  const response = await request.post("/settings/agent-keys", {
    multipart,
    headers: { Cookie: session.cookie, Accept: "application/json" },
  });
  expect(response.status(), await response.text()).toBe(201);
  return (await response.json()) as { token: string; key: { id: string; name: string; can_mint: boolean } };
}

export function bearer(token: string) {
  return { headers: { Authorization: `Bearer ${token}` } };
}

export async function signIn(page: Page) {
  await page.goto("/");
  await page.getByRole("link", { name: "Sign in with GitHub" }).click();
  await expect(page).toHaveURL(/\/dashboard$/);
}

export async function openSettings(page: Page) {
  await page.getByRole("navigation").getByRole("link", { name: "Settings" }).first().click();
  await expect(page).toHaveURL(/\/settings$/);
  await expect(page.getByRole("heading", { name: "Agent keys" })).toBeVisible();
}
