// The task table of the dock and the Tasks page: a fact in the Result column, no cell wraps, columns resize by pointer and
// keyboard and the widths survive a reload, the detail shows the whole text, and the phone keeps three columns.
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dockRow } from "../lib/ui";

const VM = `${P}tbl1`;
const VOL = `${P}tbl-dup.qcow2`;
const head = (page: import("@playwright/test").Page, label: string) => page.locator(".dock .dk-head [role=columnheader]").filter({ hasText: new RegExp(`^${label}$`) });
const width = async (page: import("@playwright/test").Page, label: string) => Math.round((await head(page, label).boundingBox())!.width);

test.describe.serial("task table", () => {
  test.afterAll(async ({ api, n1 }) => {
    await api.del(`/nodes/${n1}/pools/default/volumes/${VOL}`).catch(() => {});
    await cleanVMs(api, VM);
    await api.put("/me/prefs", { tasksDock: "open" }).catch(() => {});
  });

  test("tasks end with a fact; a failed one shows its error; nothing wraps or runs sideways", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    await api.put("/me/prefs", { tasksDock: "open" });
    await createCirros(api, n1, VM);
    const body = { name: VOL, capacity: 2 ** 20 * 64, format: "qcow2" };
    await api.post(`/nodes/${n1}/pools/default/volumes`, body);
    await api.call("POST", `/nodes/${n1}/pools/default/volumes`, body, [400, 409, 422, 500]);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    const dock = page.locator(".dock");
    const created = dockRow(page, "create", VM).first();
    await expect(created).toContainText("Ok");
    await expect(created.locator(".c-res")).not.toBeEmpty(); // a fact, not a blank
    const failed = dockRow(page, "create volume", VOL).filter({ hasText: "Error" }).first();
    await expect(failed.locator(".dk-err")).not.toBeEmpty();
    // every row is one line high, and the table does not scroll sideways
    const heights = await dock.locator(".dk-rows .dk-row").evaluateAll((rows) => rows.map((r) => Math.round(r.getBoundingClientRect().height)));
    expect(new Set(heights.slice(0, -1)).size).toBe(1); // the last row has no bottom line
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
    expect(await dock.locator(".dock-body").evaluate((e) => e.scrollWidth - e.clientWidth)).toBeLessThanOrEqual(0);
    // no ellipsis character in the cells
    expect(await dock.locator(".dk-rows").evaluate((e) => (e.textContent ?? "").includes("…"))).toBe(false);
    // the whole error is the title of the result and is in the detail
    const err = failed.locator(".dk-err");
    const full = (await err.getAttribute("title"))!;
    expect(full.length).toBeGreaterThan(5);
    await failed.click({ position: { x: 4, y: 4 } });
    const detail = page.getByRole("dialog", { name: new RegExp(`create volume default/${VOL}`) });
    await expect(detail.locator(".tdetail-result")).toHaveText(full);
    await page.keyboard.press("Escape");
    await expect(detail).toBeHidden();
  });

  test("a column is resized by pointer and keys, double click resets, widths survive a reload; Result keeps room", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    await page.evaluate(() => localStorage.removeItem("virticle-task-cols"));
    await page.reload();
    await expect(page.locator(".dock .dk-rows .dk-row").first()).toBeVisible();
    const def = await width(page, "Target");
    const grip = page.getByRole("separator", { name: "Resize Target column" });
    const b = (await grip.boundingBox())!;
    await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
    await page.mouse.down();
    await page.mouse.move(b.x + b.width / 2 + 30, b.y + b.height / 2);
    await page.mouse.move(b.x + b.width / 2 + 60, b.y + b.height / 2);
    await page.mouse.up();
    expect(await width(page, "Target")).toBe(def + 60);
    await page.reload();
    await expect(page.locator(".dock .dk-rows .dk-row").first()).toBeVisible();
    expect(await width(page, "Target")).toBe(def + 60);
    // keyboard: arrows move by 8, shift by 32, Home resets
    await grip.focus();
    await page.keyboard.press("ArrowLeft");
    expect(await width(page, "Target")).toBe(def + 52);
    await page.keyboard.press("Shift+ArrowRight");
    expect(await width(page, "Target")).toBe(def + 84);
    await page.keyboard.press("Home");
    expect(await width(page, "Target")).toBe(def);
    // too wide a drag stops where Result still has its minimum, and nothing scrolls sideways
    const b2 = (await grip.boundingBox())!;
    await page.mouse.move(b2.x + b2.width / 2, b2.y + b2.height / 2);
    await page.mouse.down();
    await page.mouse.move(b2.x + 900, b2.y + b2.height / 2);
    await page.mouse.up();
    expect(await page.locator(".dock .dk-head .c-res").evaluate((e) => Math.round(e.getBoundingClientRect().width))).toBeGreaterThanOrEqual(120);
    expect(await page.locator(".dock-body").evaluate((e) => e.scrollWidth - e.clientWidth)).toBeLessThanOrEqual(0);
    await grip.dblclick();
    expect(await width(page, "Target")).toBe(def);
    // the Tasks page shares the widths
    await grip.focus();
    await page.keyboard.press("ArrowRight");
    await page.goto("/datacenter/tasks");
    await expect(page.locator("main .taskpage .dk-head [role=columnheader]").filter({ hasText: /^Target$/ })).toBeVisible();
    expect(Math.round((await page.locator("main .taskpage .dk-head [role=columnheader]").filter({ hasText: /^Target$/ }).boundingBox())!.width)).toBe(def + 8);
    await page.evaluate(() => localStorage.removeItem("virticle-task-cols"));
  });

  test("phone width: status, target and result, no grips, nothing sideways", async ({ page, api }) => {
    await api.put("/me/prefs", { tasksDock: "open" });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/datacenter");
    const dock = page.locator(".dock");
    await expect(dock.locator(".dk-head .dk-cell").filter({ hasText: /^Result$/ })).toBeVisible();
    await expect(dock.getByRole("separator")).toHaveCount(0);
    for (const h of ["Action", "Node", "By", "Started", "Duration"]) await expect(dock.locator(".dk-head .dk-cell").filter({ hasText: new RegExp(`^${h}$`) })).toBeHidden();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
    // a row opens the detail, which has what the phone leaves out
    await dock.locator(".dk-rows .dk-row").first().click({ position: { x: 4, y: 4 } });
    await expect(page.getByRole("dialog").locator("dt", { hasText: "Started" })).toBeVisible();
  });
});
