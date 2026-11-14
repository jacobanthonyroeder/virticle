// Node page: opened from the Nodes list, every tab renders, host interfaces and the default pool are shown.
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";

const TABS: [string, string][] = [
  ["Summary", "summary"],
  ["Virtual machines", "vms"],
  ["Storage", "storage"],
  ["Network", "network"],
  ["Updates", "updates"],
  ["Settings", "settings"],
];

test.describe.serial("node page", () => {
  test("opens from the node list and every tab renders without an error", async ({ page }) => {
    await page.goto("/nodes");
    await page.locator("tbody tr").filter({ hasText: env.node1 }).getByRole("link", { name: env.node1 }).click();
    await expect(page).toHaveURL(/\/node\/\d+$/);
    await expect(page.getByRole("heading", { name: env.node1, level: 1 })).toBeVisible();
    await expect(page.locator(".nodetitle .badge", { hasText: "online" })).toBeVisible();
    for (const [label, slug] of TABS) {
      await page.getByRole("tab", { name: label, exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`/node/\\d+/${slug}$`));
      await expect(page.getByRole("tab", { name: label, exact: true })).toHaveAttribute("aria-selected", "true");
      await page.waitForTimeout(1500);
      await expect(page.locator(".error")).toHaveCount(0);
    }
  });

  test("summary shows host facts", async ({ page }) => {
    await page.goto("/nodes");
    await page.locator("main").getByRole("link", { name: env.node1 }).click();
    await expect(page.locator(".card", { hasText: "Host" }).first()).toContainText(/\d+ ×/);
    await expect(page.locator(".card", { hasText: "Inventory" })).toContainText(/\d+ of \d+/);
  });

  test("storage tab shows the default pool, network tab lists host interfaces", async ({ page }) => {
    await page.goto("/nodes");
    await page.locator("main").getByRole("link", { name: env.node1 }).click();
    await page.getByRole("tab", { name: "Storage", exact: true }).click();
    await expect(page.locator("tbody tr").filter({ hasText: "default" }).first()).toBeVisible();
    await page.getByRole("tab", { name: "Network", exact: true }).click();
    await page.getByRole("button", { name: "Host interfaces" }).click();
    await expect(page.locator(".nicbox tbody tr").first()).toBeVisible();
    expect(await page.locator(".nicbox tbody tr").count()).toBeGreaterThan(0);
  });

  test("updates tab lists every available package in a table without a click", async ({ page, api, n1 }) => {
    const u = await api.get(`/nodes/${n1}/updates`);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto(`/node/${n1}/updates`);
    const box = page.locator(".hostupdates");
    if (u.updates.length === 0) {
      await expect(box.locator(".upd-count")).toHaveText("Up to date");
      await expect(box.locator(".empty")).toBeVisible();
    } else {
      await expect(box.locator(".upd-count")).toHaveText(new RegExp(`^${u.updates.length} updates? available$`));
      await expect(box.locator(".upd-table thead th")).toContainText(["Package", "Installed", "Available"]);
      await expect(box.locator(".upd-table tbody tr")).toHaveCount(u.updates.length);
      await expect(box.locator(".upd-table tbody tr").first()).toContainText(u.updates[0].name);
      // the table uses the width of the page, not a narrow column
      expect((await box.locator(".upd-table").boundingBox())!.width).toBeGreaterThan(600);
    }
    await expect(box.getByRole("button", { name: "Check again" })).toBeVisible();
    await expect(box.getByRole("button", { name: "Update this node" })).toBeVisible();
    await page.screenshot({ path: "test-results/visual/node-updates.png", fullPage: true });
  });

  test("an unknown node shows the empty state", async ({ page }) => {
    await page.goto("/node/99999");
    await expect(page.getByText("Not found", { exact: true })).toBeVisible();
  });

  test("no sideways scroll at 390px on any tab", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/nodes");
    await page.locator("main").getByRole("link", { name: env.node1 }).click();
    for (const [label] of TABS) {
      await page.getByRole("tab", { name: label, exact: true }).click();
      await page.waitForTimeout(1200);
      const wide = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(wide, `${label} overflows`).toBeLessThanOrEqual(0);
    }
  });
});
