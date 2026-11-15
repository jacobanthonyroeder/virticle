// The Migrate dialog sends the request and closes once the server accepted it (no "Run in background" button, no progress in
// the dialog); a refused request keeps it open with the error; a migration that fails later reaches the user as a toast that
// stays. A second node and the migration answers are made up with page.route, as in spec 56; the events come through the page's
// own socket.
import { Page, WebSocketRoute } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, dockRow, vmAction } from "../lib/ui";

const NAME = `${P}mig-close`;

async function fakeSecondNode(page: Page) {
  await page.route("**/api/nodes", async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    const res = await route.fetch();
    const body = await res.json();
    const first = body.nodes[0];
    body.nodes.push({ ...first, id: 9999, addr: "node-two.invalid", hostname: "node-two", state: "online" });
    await route.fulfill({ response: res, json: body });
  });
  await page.route("**/migrate/check", (route) => route.fulfill({ json: { ok: true, mode: "live", checks: [{ check: "CPU", ok: true, severity: "info", message: "compatible" }], copyBytes: 0, mirrorBytes: 0, encrypt: true } }));
}

test.describe.serial("migrate dialog", () => {
  test.afterAll(async ({ api }) => cleanVMs(api, NAME, `${NAME}-copy`, `${NAME}-src`));

  test("closes once accepted; a refusal keeps it open; a late failure is a toast that stays", async ({ page, api, n1 }) => {
    test.setTimeout(120_000);
    const vm = await createCirros(api, n1, NAME, { start: true });
    const sockets: WebSocketRoute[] = [];
    await page.routeWebSocket(/\/api\/events/, (ws) => {
      const server = ws.connectToServer();
      server.onMessage((m) => ws.send(m));
      ws.onMessage((m) => server.send(m));
      sockets.push(ws);
    });
    await fakeSecondNode(page);
    let refuse = true;
    let sent: any;
    await page.route("**/api/nodes/*/vms/*/migrate", async (route) => {
      if (route.request().method() !== "POST") return route.continue();
      sent = route.request().postDataJSON();
      if (refuse) return route.fulfill({ status: 400, json: { error: "the target has no room" } });
      await route.fulfill({ json: { task: 987654 } });
    });
    await openVM(page, vm);
    await vmAction(page, "Migrate", "Migrate to another node");
    const dlg = dialog(page, `Migrate ${NAME}`);
    const go = dlg.getByRole("button", { name: "Migrate", exact: true });
    await expect(go).toBeEnabled({ timeout: 30_000 });
    await expect(dlg.getByRole("button", { name: "Run in background" })).toHaveCount(0);
    await go.click();
    await expect(dlg.getByText("the target has no room", { exact: false })).toBeVisible(); // refused: the dialog stays with the error
    refuse = false;
    await go.click();
    await expect(dlg).toBeHidden();
    expect(sent).toMatchObject({ target: 9999 });
    // 30 s later it fails: the user who started it gets a toast that stays, with links
    const task = { id: 987654, kind: "migrate", target: `${NAME} → node-two`, node: "", user: "admin", status: "running", progress: 0.4, started: Math.floor(Date.now() / 1000) - 30, background: true };
    sockets.forEach((s) => s.send(JSON.stringify({ type: "task", task })));
    await expect(dockRow(page, "migrate", NAME).first()).toContainText("Running");
    sockets.forEach((s) => s.send(JSON.stringify({ type: "task", task: { ...task, status: "failed", error: "the target refused the memory stream", finished: Math.floor(Date.now() / 1000) } })));
    const toast = page.locator(".toast", { hasText: "migrate" });
    await expect(toast).toContainText("the target refused the memory stream");
    await page.waitForTimeout(12_000);
    await expect(toast).toBeVisible();
    await expect(dockRow(page, "migrate", NAME).first()).toContainText("Error");
    await toast.getByRole("button", { name: "Dismiss" }).click();
    await expect(toast).toHaveCount(0);
  });

  test("the Clone dialog closes once the server accepted the clone; the dock says what was made", async ({ page, api, n1 }) => {
    test.setTimeout(120_000);
    const vm = await createCirros(api, n1, `${NAME}-src`);
    await openVM(page, vm);
    await vmAction(page, "Clone");
    const dlg = dialog(page, `Clone ${NAME}-src`);
    await dlg.getByLabel("Name of the clone").fill(`${NAME}-copy`);
    await dlg.getByRole("button", { name: "Clone", exact: true }).click();
    await expect(dlg).toBeHidden();
    const row = dockRow(page, "clone", `${NAME}-copy`).first();
    await expect(row).toContainText("Ok", { timeout: 90_000 });
    await expect(row.locator(".c-res")).toContainText(`${NAME}-copy, full copy`);
    expect(await api.vmByName(`${NAME}-copy`)).toBeTruthy();
  });
});
