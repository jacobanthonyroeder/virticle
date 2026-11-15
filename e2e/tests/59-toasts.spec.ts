// Toasts: the failed task of the signed-in user stays until dismissed and links to the task; one of another user is timed;
// a raised alert shows once (the alert of a failed task adds no second toast); at most four show, with "N more".
// Alerts are raised by the server every minute, so the alert events are sent to the page through its own socket.
import { Page, WebSocketRoute } from "@playwright/test";
import { Api } from "../lib/api";
import { env, P } from "../lib/env";
import { expect, test } from "../lib/fixtures";

const VOL = `${P}toast-dup.qcow2`;
const DL = `${P}toast-dl.img`;
const OTHER = `${P}toastop`;
const PASS = "e2e-toast-pass-123";

async function socket(page: Page) {
  const sockets: WebSocketRoute[] = [];
  await page.routeWebSocket(/\/api\/events/, (ws) => {
    const server = ws.connectToServer();
    server.onMessage((m) => ws.send(m));
    ws.onMessage((m) => server.send(m));
    sockets.push(ws);
  });
  return (e: object) => sockets.forEach((s) => s.send(JSON.stringify(e)));
}
/** A task that fails after its request was answered: the node cannot fetch this URL. */
const failInBackground = (api: Api, n1: number, who = api) => who.call("POST", `/nodes/${n1}/pools/default/volumes/download?async=1`, { name: DL, url: "http://127.0.0.1:9/none.img" }, [202]);
const alert = (id: string, severity: "warning" | "critical", extra: object = {}) => ({
  type: "alert",
  detail: "raised",
  alert: { id, kind: "pool-usage", severity, title: `Alert ${id}`, message: `Message of ${id}`, since: Math.floor(Date.now() / 1000), ...extra },
});

test.describe.serial("toasts", () => {
  test.afterAll(async ({ api, n1 }) => {
    await api.del(`/nodes/${n1}/pools/default/volumes/${VOL}`).catch(() => {});
    for (const u of await api.get("/users")) if (u.username === OTHER) await api.del(`/users/${u.id}`).catch(() => {});
  });

  test("your failed task stays until dismissed and links to the task; another user's is timed", async ({ page, api, n1 }) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    const body = { name: VOL, capacity: 2 ** 20 * 64, format: "qcow2" };
    await api.post(`/nodes/${n1}/pools/default/volumes`, body);
    // a task the request waited for: its error is in the page that asked, so its toast is timed
    await api.call("POST", `/nodes/${n1}/pools/default/volumes`, body, [400, 409, 422, 500]);
    const quick = page.locator(".toast", { hasText: `create volume default/${VOL} failed` });
    await expect(quick).toHaveCount(1);
    await expect(quick).toHaveCount(0, { timeout: 20_000 });
    // a task that runs on after the request was answered: the toast stays
    await failInBackground(api, n1);
    const toast = page.locator(".toast", { hasText: `download default/${DL} failed` });
    await expect(toast).toHaveCount(1);
    await expect(toast).toHaveAttribute("role", "alert");
    await page.waitForTimeout(12_000); // longer than the timed toasts
    await expect(toast).toBeVisible();
    // links to the task and its target
    await toast.getByRole("link", { name: "Task" }).click();
    await expect(page).toHaveURL(/\/datacenter\/tasks\?task=\d+/);
    await expect(page.getByRole("dialog", { name: new RegExp(`download default/${DL}`) })).toBeVisible();
    await page.keyboard.press("Escape");
    // dismissed with its own button, reachable by keyboard
    const x = toast.getByRole("button", { name: "Dismiss" });
    await x.focus();
    await page.keyboard.press("Enter");
    await expect(toast).toHaveCount(0);

    // a task of another user is timed
    if (!(await api.get("/users")).some((u: any) => u.username === OTHER)) await api.post("/users", { username: OTHER, password: PASS, role: "operator" });
    const other = await Api.login(OTHER, PASS);
    await failInBackground(other, n1);
    await expect(toast).toHaveCount(1);
    await expect(toast).toHaveCount(0, { timeout: 20_000 });
    await other.ctx.dispose();
  });

  test("a raised alert shows once; the alert of a failed task adds no second toast; an error stays, a warning goes", async ({ page, api, n1 }) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width: 1280, height: 720 });
    const send = await socket(page);
    await page.goto("/datacenter");
    await expect(page.locator(".dock-bar")).toBeVisible();
    await expect.poll(async () => { send(alert("probe", "warning")); return page.locator(".toast", { hasText: "Alert probe" }).count(); }, { timeout: 15_000 }).toBeGreaterThan(0);
    await expect(page.locator(".toast", { hasText: "Alert probe" })).toHaveCount(1);
    await expect(page.locator(".toast", { hasText: "Alert probe" })).toHaveCount(0, { timeout: 20_000 }); // warnings are timed

    send(alert("down1", "critical"));
    const err = page.locator(".toast", { hasText: "Alert down1" });
    await expect(err).toHaveCount(1);
    await expect(err).toHaveAttribute("role", "alert");
    await expect(err.getByRole("button", { name: "Open alerts" })).toBeVisible();
    await page.waitForTimeout(12_000);
    await expect(err).toBeVisible();
    await err.getByRole("button", { name: "Dismiss" }).click();
    await expect(err).toHaveCount(0);

    // a failed task of this user and the alert the server raises for it: one toast
    await failInBackground(api, n1);
    await expect(page.locator(".toast", { hasText: "failed" })).toHaveCount(1);
    send(alert(`job:download:${env.node1}:default`, "warning", { kind: "failed-job", user: env.user, title: `Download of default/${DL} failed` }));
    await page.waitForTimeout(1000);
    await expect(page.locator(".toast")).toHaveCount(1);
    await page.locator(".toast .toast-x").click();
  });

  test("at most four show, the rest are counted and open the alerts", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 720 });
    const send = await socket(page);
    await page.goto("/datacenter");
    await expect.poll(async () => { send(alert("c0", "critical")); return page.locator(".toast").count(); }, { timeout: 15_000 }).toBe(1);
    for (let i = 1; i < 6; i++) send(alert(`c${i}`, "critical"));
    await expect(page.locator(".toast")).toHaveCount(4);
    const more = page.getByRole("button", { name: "2 more" });
    await expect(more).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
    await more.click();
    await expect(page.getByRole("dialog", { name: "Alerts" })).toBeVisible();
    await page.keyboard.press("Escape");
    for (let i = 0; i < 4; i++) await page.locator(".toast .toast-x").first().click();
    await expect(page.locator(".toast")).toHaveCount(2);
    await page.locator(".toast .toast-x").first().click();
    await page.locator(".toast .toast-x").first().click();
    await expect(page.locator(".toast")).toHaveCount(0);
  });

  test("phone width: a toast fits and nothing runs sideways", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const send = await socket(page);
    await page.goto("/datacenter");
    await expect.poll(async () => { send(alert("ph", "critical", { message: "A long message ".repeat(12) })); return page.locator(".toast").count(); }, { timeout: 15_000 }).toBe(1);
    const box = (await page.locator(".toast").boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(390);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
    await page.locator(".toast .toast-x").click();
  });
});
