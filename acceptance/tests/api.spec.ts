import { APIRequestContext, expect, test } from "@playwright/test";
import { bearer, browserSession, settingsCreateKey, slug, unique } from "./support";

// The agent and ingestion API end to end against the real server and store.

async function ordinaryKey(request: APIRequestContext, name: string, user: "owner" | "other" = "owner") {
  const session = await browserSession(request, user);
  return (await settingsCreateKey(request, session, name)).token;
}

async function newProject(request: APIRequestContext, token: string, name: string) {
  const response = await request.post("/v1/projects", { ...bearer(token), data: { name, slug: slug(name) } });
  expect(response.status(), await response.text()).toBe(201);
  return response.json() as Promise<{ id: string; slug: string; name: string }>;
}

async function ingestionToken(request: APIRequestContext, token: string, projectID: string) {
  const response = await request.post(`/v1/projects/${projectID}/ingestion-tokens`, bearer(token));
  expect(response.status(), await response.text()).toBe(201);
  return response.json() as Promise<{ token: string; credential: { id: string } }>;
}

function notice(message: string) {
  return {
    env: "production",
    revision: "acceptance",
    handled: true,
    error: {
      class: "*errors.errorString",
      message,
      backtrace: ["app/internal/importer/run.go:42 in example.com/widget/internal/importer.Run"],
    },
    context: { import_id: 7 },
  };
}

test("bootstrap exchange returns a working key once", async ({ request }) => {
  const session = await browserSession(request);
  const created = await request.post("/settings/bootstrap", {
    multipart: { csrf: session.csrf },
    headers: { Cookie: session.cookie, Accept: "application/json" },
  });
  expect(created.status()).toBe(201);
  const { prompt } = await created.json();
  expect(prompt).toContain("/llms.txt");
  const bootstrap = prompt.match(/boot_[A-Za-z0-9_-]+/)[0];

  const exchange = await request.post("/v1/bootstrap/exchange", { data: { token: bootstrap, name: "API bootstrap" } });
  expect(exchange.status(), await exchange.text()).toBe(201);
  const { token, key } = await exchange.json();
  expect(token).toMatch(/^raises_agent_/);
  expect(key.name).toBe("API bootstrap");
  expect((await request.get("/v1/projects", bearer(token))).status()).toBe(200);

  const again = await request.post("/v1/bootstrap/exchange", { data: { token: bootstrap, name: "Replay" } });
  expect(again.status()).toBeGreaterThanOrEqual(400);
  expect(again.status()).toBeLessThan(500);
});

test("account keys mint, list and revoke agent keys", async ({ request }, info) => {
  const session = await browserSession(request);
  const account = await settingsCreateKey(request, session, unique(info, "Account host"), true);
  expect(account.key.can_mint).toBe(true);

  const mintedName = unique(info, "Minted");
  const mint = await request.post("/v1/agent-keys", { ...bearer(account.token), data: { name: mintedName } });
  expect(mint.status(), await mint.text()).toBe(201);
  const minted = await mint.json();
  expect((await request.get("/v1/projects", bearer(minted.token))).status()).toBe(200);

  const list = await request.get("/v1/agent-keys", bearer(account.token));
  expect(list.status()).toBe(200);
  const listed = (await list.json()).find((k: any) => k.id === minted.key.id);
  expect(listed).toMatchObject({ name: mintedName, minted_by_key_id: account.key.id });

  expect((await request.get("/v1/agent-keys", bearer(minted.token))).status()).toBe(403);
  expect((await request.get("/v1/agent-keys", bearer("raises_agent_unknown"))).status()).toBe(401);

  const revoke = await request.delete(`/v1/agent-keys/${minted.key.id}`, bearer(account.token));
  expect(revoke.status()).toBeLessThan(300);
  expect((await request.get("/v1/projects", bearer(minted.token))).status()).toBe(401);
});

test("projects, ingestion tokens, notices and the error lifecycle", async ({ request }, info) => {
  const token = await ordinaryKey(request, unique(info, "Error lifecycle"));
  const project = await newProject(request, token, unique(info, "Widget"));

  const projects = await request.get("/v1/projects", bearer(token));
  expect((await projects.json()).map((p: any) => p.id)).toContain(project.id);
  const renamed = await request.patch(`/v1/projects/${project.id}`, { ...bearer(token), data: { name: "Renamed widget" } });
  expect(await renamed.json()).toMatchObject({ id: project.id, name: "Renamed widget" });

  const ingestion = await ingestionToken(request, token, project.id);
  const first = await request.post("/v1/notices", { ...bearer(ingestion.token), data: notice("import failed") });
  expect(first.status(), await first.text()).toBe(201);
  const second = await request.post("/v1/notices", { ...bearer(ingestion.token), data: notice("import failed") });
  expect(second.status()).toBe(200);
  const group = (await first.json()).Group;
  expect(group).toMatchObject({ app: project.slug, message: "import failed" });

  const detail = await request.get(`/v1/errors/${group.id}`, bearer(token));
  expect(await detail.json()).toMatchObject({ id: group.id, count: 2, status: "open" });
  const notices = await request.get(`/v1/errors/${group.id}/notices?limit=5`, bearer(token));
  expect(await notices.json()).toHaveLength(2);

  const open = async () => (await (await request.get(`/v1/errors?app=${project.slug}&unacked=1`, bearer(token))).json()).map((g: any) => g.id);
  expect(await open()).toContain(group.id);

  const ack = await request.post(`/v1/errors/${group.id}/ack`, bearer(token));
  expect((await ack.json()).acked_at).toBeTruthy();
  expect(await open()).not.toContain(group.id);

  const suppress = await request.post(`/v1/errors/${group.id}/suppress`, bearer(token));
  expect((await suppress.json()).suppressed_at).toBeTruthy();
  const openPage = await request.get(`/v1/errors?app=${project.slug}&status=open`, bearer(token));
  expect((await openPage.json()).errors.map((g: any) => g.id)).not.toContain(group.id);
  const allPage = await request.get(`/v1/errors?app=${project.slug}&status=all`, bearer(token));
  expect((await allPage.json()).errors.map((g: any) => g.id)).toContain(group.id);
  const unsuppress = await request.post(`/v1/errors/${group.id}/unsuppress`, bearer(token));
  expect((await unsuppress.json()).suppressed_at).toBeFalsy();

  // A revoked ingestion token stops ingesting; an archived project too.
  const revoke = await request.delete(`/v1/projects/${project.id}/ingestion-tokens/${ingestion.credential.id}`, bearer(token));
  expect(revoke.status()).toBeLessThan(300);
  expect((await request.post("/v1/notices", { ...bearer(ingestion.token), data: notice("after revoke") })).status()).toBe(401);
  const fresh = await ingestionToken(request, token, project.id);
  expect((await request.post(`/v1/projects/${project.id}/archive`, bearer(token))).status()).toBe(200);
  expect((await request.post("/v1/notices", { ...bearer(fresh.token), data: notice("after archive") })).status()).toBe(401);
  expect((await request.post(`/v1/projects/${project.id}/restore`, bearer(token))).status()).toBe(200);
});

test("one account cannot see another account's projects or errors", async ({ request }, info) => {
  const owner = await ordinaryKey(request, unique(info, "Owner"));
  const other = await ordinaryKey(request, unique(info, "Other"), "other");
  const project = await newProject(request, owner, unique(info, "Private"));
  const ingestion = await ingestionToken(request, owner, project.id);
  const group = (await (await request.post("/v1/notices", { ...bearer(ingestion.token), data: notice("private") })).json()).Group;

  expect((await request.get(`/v1/projects/${project.id}`, bearer(other))).status()).toBe(404);
  expect((await request.get(`/v1/errors/${group.id}`, bearer(other))).status()).toBe(404);
  expect((await request.post(`/v1/errors/${group.id}/ack`, bearer(other))).status()).toBe(404);
});

test("webhook endpoints receive test, error and notice events", async ({ request }, info) => {
  const token = await ordinaryKey(request, unique(info, "Webhooks"));
  const project = await newProject(request, token, unique(info, "Hooked"));

  const insecure = await request.post("/v1/webhook-endpoints", { ...bearer(token), data: { url: "http://hooks.example.com/raises" } });
  expect(insecure.status()).toBe(400);
  expect((await insecure.json()).error).toContain("HTTPS");

  const created = await request.post("/v1/webhook-endpoints", {
    ...bearer(token),
    data: { url: "https://hooks.example.com/raises", events: ["error.created", "notice.created"], authorization_header: "Authorization", authorization_value: "Bearer destination-secret" },
  });
  expect(created.status(), await created.text()).toBe(201);
  const { endpoint, signing_secret } = await created.json();
  expect(signing_secret).toBeTruthy();

  const listed = await (await request.get("/v1/webhook-endpoints", bearer(token))).json();
  const row = listed.find((e: any) => e.id === endpoint.id);
  expect(row).toMatchObject({ authorization_header: "Authorization" });
  expect(JSON.stringify(listed)).not.toContain("destination-secret");
  expect(JSON.stringify(listed)).not.toContain(signing_secret);

  const recorded = async () => (await (await request.get(`/__acceptance/deliveries?endpoint_id=${endpoint.id}`)).json()).map((d: any) => d.type);
  expect((await request.post(`/v1/webhook-endpoints/${endpoint.id}/test`, bearer(token))).status()).toBeLessThan(300);
  await expect.poll(recorded).toContain("webhook.test");

  const ingestion = await ingestionToken(request, token, project.id);
  expect((await request.post("/v1/notices", { ...bearer(ingestion.token), data: notice("hooked") })).status()).toBe(201);
  await expect.poll(recorded).toContain("error.created");
  const event = await request.post("/v1/events", { ...bearer(ingestion.token), data: { env: "production", message: "Import finished", source: "acceptance" } });
  expect(event.status(), await event.text()).toBe(201);
  await expect.poll(recorded).toContain("notice.created");

  const rotated = await request.post(`/v1/webhook-endpoints/${endpoint.id}/rotate-secret`, bearer(token));
  const rotatedSecret = (await rotated.json()).signing_secret;
  expect(rotatedSecret).toBeTruthy();
  expect(rotatedSecret).not.toBe(signing_secret);

  const paused = await request.patch(`/v1/webhook-endpoints/${endpoint.id}`, {
    ...bearer(token),
    data: { url: "https://hooks.example.com/raises", events: ["error.created"], active: false },
  });
  expect(await paused.json()).toMatchObject({ id: endpoint.id, active: false });

  const deliveries = await request.get("/v1/webhook-deliveries?limit=50", bearer(token));
  expect(deliveries.status()).toBe(200);

  expect((await request.delete(`/v1/webhook-endpoints/${endpoint.id}`, bearer(token))).status()).toBeLessThan(300);
  const after = await (await request.get("/v1/webhook-endpoints", bearer(token))).json();
  expect(after.map((e: any) => e.id)).not.toContain(endpoint.id);
});
