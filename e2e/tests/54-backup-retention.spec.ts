// The schedule assistant on VM > Backups > Schedule and on Settings > Controller backup: presets fill the keep
// fields, a rule that cannot have an effect is disabled, the server's projection appears and follows the fields,
// nothing scrolls sideways on a phone; for a running VM, weekly and monthly points are full backups (no chains kept for
// them). The controller's schedule is looked at, not saved.
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { field, pick } from "../lib/ui";

const NAME = `${P}ret1`;

const keepField = (page: Page, label: string) => page.getByRole("group", { name: "Keep" }).getByRole("spinbutton", { name: label, exact: true });
const kept = (page: Page) => page.getByRole("region", { name: "Projection" }).getByRole("status");

async function noSideways(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

test.describe.serial("backup schedule assistant", () => {
  let vm: { node: number; uuid: string; name: string };
  const sched = () => `/nodes/${vm.node}/vms/${vm.uuid}/backup-schedule`;

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
  });
  test.afterAll(async ({ api }) => {
    await api.del(sched()).catch(() => {});
    await cleanVMs(api, NAME);
  });

  test("presets fill the fields, hourly is off for a daily schedule, the projection follows", async ({ page, api }) => {
    await openVM(page, vm, "backups", "schedule");
    const sec = page.locator("#backup-schedule");
    await sec.getByRole("button", { name: "Set up schedule" }).click();
    await expect(kept(page)).toContainText(/Kept: \d+ backups?, back to /, { timeout: 30_000 });
    // a daily schedule: hourly cannot have an effect
    await expect(keepField(page, "Hourly")).toBeDisabled();
    await expect(page.locator("label.sa-n", { hasText: "Hourly" })).toHaveAttribute("title", /at most once a day/);
    const before = await kept(page).textContent();
    await sec.getByRole("button", { name: "Standard", exact: true }).click();
    await expect(keepField(page, "Daily")).toHaveValue("7");
    await expect(keepField(page, "Weekly")).toHaveValue("4");
    await expect(keepField(page, "Monthly")).toHaveValue("6");
    await expect(keepField(page, "Hourly")).toHaveValue("0");
    await expect(kept(page)).not.toHaveText(before ?? "", { timeout: 15_000 });
    await expect(page.getByRole("region", { name: "Projection" }).locator("table")).toContainText("Monthly");
    await expect(page.getByRole("region", { name: "Projection" })).toContainText("Space: about");
    await expect(page.getByRole("region", { name: "Projection" })).toContainText("No backup yet: estimate from disk usage.");
    // every 6 hours: hourly counts again and keeps its preset value
    await pick(sec, "When", "At a fixed interval");
    await expect(keepField(page, "Hourly")).toBeEnabled();
    await expect(keepField(page, "Hourly")).toHaveValue("24");
    await sec.getByRole("spinbutton", { name: "Every", exact: true }).fill("6");
    await expect(page.getByRole("region", { name: "Projection" }).locator("table")).toContainText("Hourly", { timeout: 15_000 });
    // a field change changes the numbers
    const t1 = await kept(page).textContent();
    await keepField(page, "Monthly").fill("12");
    await expect(kept(page)).not.toHaveText(t1 ?? "", { timeout: 15_000 });
    await sec.getByRole("button", { name: "Long", exact: true }).click();
    await expect(keepField(page, "Hourly")).toHaveValue("0");
    await expect(keepField(page, "Weekly")).toHaveValue("8");
    await sec.getByRole("button", { name: "Short", exact: true }).click();
    await expect(keepField(page, "Hourly")).toHaveValue("24");
    await expect(keepField(page, "Monthly")).toHaveValue("0");
    await sec.getByRole("button", { name: "Save schedule" }).click();
    await expect(sec).toContainText("Keep: 24 hourly, 7 daily.");
    const got = await api.get(sched());
    expect(got.schedule).toMatchObject({ mode: "interval", everyMinutes: 360, keepLast: 0, keepHourly: 24, keepDaily: 7, keepWeekly: 0, keepMonthly: 0 });
    // saved as daily: the hourly rule is stored as 0
    await sec.getByRole("button", { name: "Edit schedule" }).click();
    await pick(sec, "When", "Daily at a fixed time");
    await expect(field(sec, "Time")).toHaveValue(/^\d\d:\d\d$/);
    await sec.getByRole("button", { name: "Save schedule" }).click();
    await expect(sec).toContainText("Keep: 7 daily.");
    expect((await api.get(sched())).schedule).toMatchObject({ mode: "daily", keepHourly: 0, keepDaily: 7 });
  });

  test("the server refuses a schedule that keeps nothing and keeps an old keep-last schedule as it was", async ({ api }) => {
    const bad = await api.ctx.fetch(`/api${sched()}/preview`, { method: "POST", data: { mode: "daily", at: "02:00", pool: "default", keepLast: 0, keepHourly: 24 } });
    expect(bad.status()).toBe(400);
    await api.put(sched(), { mode: "daily", at: "02:00", pool: "default", targetNode: 0, keepLast: 5 });
    expect((await api.get(sched())).schedule).toMatchObject({ keepLast: 5, keepHourly: 0, keepDaily: 0 });
  });

  test("the Back up now dialog uses the schedule's rules for the schedule's pool", async ({ page }) => {
    await openVM(page, vm, "backups");
    await page.getByRole("button", { name: "Back up now" }).first().click();
    const dlg = page.getByRole("dialog", { name: `Back up ${NAME}` });
    await expect(dlg).toContainText("Retention: schedule (last 5).");
    await expect(field(dlg, "Keep the newest")).toHaveCount(0);
    await page.keyboard.press("Escape");
  });

  test("a running VM: weekly and monthly points are full, so they keep no chain", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    const pr = await api.post(`${sched()}/preview`, { mode: "daily", at: "02:00", pool: "default", targetNode: 0, keepDaily: 7, keepWeekly: 4, keepMonthly: 6 });
    expect(pr.chains).toBe(true);
    // every point a weekly or monthly rule keeps is full, except the newest (its week is not over yet)
    const long = (pr.points as any[]).slice(1).filter((p) => p.rules?.includes("weekly") || p.rules?.includes("monthly"));
    expect(long.length).toBeGreaterThan(3);
    for (const p of long) expect(p.full, `point ${new Date(p.time * 1000).toISOString()}`).toBe(true);
    expect((pr.points as any[]).filter((p) => p.reason === "needed")).toHaveLength(0);
    // hourly points stay incremental and keep their chains
    const hourly = await api.post(`${sched()}/preview`, { mode: "interval", everyMinutes: 60, pool: "default", targetNode: 0, keepHourly: 24, keepDaily: 7, keepWeekly: 4 });
    expect((hourly.points as any[]).filter((p) => p.reason === "needed").length).toBeGreaterThan(0);
    // the assistant shows no Chains row for the Standard preset on a daily schedule
    await openVM(page, vm, "backups", "schedule");
    const sec = page.locator("#backup-schedule");
    await sec.getByRole("button", { name: "Edit schedule" }).click();
    await pick(sec, "When", "Daily at a fixed time");
    await sec.getByRole("button", { name: "Standard", exact: true }).click();
    const table = page.getByRole("region", { name: "Projection" }).locator("table");
    await expect(table).toContainText("Monthly", { timeout: 30_000 });
    await expect(table).not.toContainText("Chains");
    await expect(page.getByRole("region", { name: "Projection" })).toContainText(/\d+ full, \d+ incremental/);
    await sec.getByRole("button", { name: "Cancel" }).click();
  });

  test("phone width: the assistant fits", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openVM(page, vm, "backups", "schedule");
    await page.locator("#backup-schedule").getByRole("button", { name: "Edit schedule" }).click();
    await page.locator("#backup-schedule").getByRole("button", { name: "Standard", exact: true }).click();
    await expect(kept(page)).toContainText("Kept:", { timeout: 30_000 });
    await noSideways(page);
    const box = await page.getByRole("group", { name: "Keep" }).boundingBox();
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  });
});

test.describe("controller backup assistant", () => {
  test("the schedule form shows the projection; nothing is saved", async ({ page, api }) => {
    const before = await api.get("/controller-backup");
    await page.goto("/controller-backup");
    const card = page.getByRole("group", { name: "Backups on this server" });
    await expect(card.getByRole("checkbox", { name: "Keep backups on this server" })).toBeVisible();
    await card.getByRole("button", { name: "Edit schedule" }).click();
    await expect(kept(page)).toContainText("Kept:", { timeout: 30_000 });
    await card.getByRole("button", { name: "Long", exact: true }).click();
    await expect(keepField(page, "Monthly")).toHaveValue("12");
    await expect(page.getByRole("region", { name: "Projection" }).locator("table")).toContainText("Monthly", { timeout: 15_000 });
    await pick(card, "When", "At a fixed interval");
    await expect(card.getByText("hours", { exact: true })).toBeVisible();
    await card.getByRole("button", { name: "Cancel" }).click();
    expect(await api.get("/controller-backup")).toEqual(expect.objectContaining({ daily: before.daily, mode: before.mode, keepDaily: before.keepDaily }));
    await page.setViewportSize({ width: 390, height: 844 });
    await card.getByRole("button", { name: "Edit schedule" }).click();
    await expect(kept(page)).toContainText("Kept:", { timeout: 30_000 });
    await noSideways(page);
  });
});
