// The sub page column inside the long tabs (VM Hardware, Snapshots, Backups; node Settings): the entries, one sub
// page at a time, the choice in the URL (reload, back, links), the keyboard, a phone, and a short window.
import { cleanVMs, createCirros, expect, openVM, subPage, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { vmAction } from "../lib/ui";

const NAME = `${P}sub1`;

test.describe.serial("sub page column", () => {
  let vm: { node: number; uuid: string; name: string };
  const entries = (page: any) => page.locator(".subnav").getByRole("tab");
  const selected = (page: any) => page.locator(".subnav").getByRole("tab", { selected: true });

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, NAME);
  });

  test("Hardware lists its sub pages and shows the first one; an unknown one is the first", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await expect(entries(page)).toHaveText(["CPU and memory", "Disks", "Network interfaces", "Boot and firmware", "Advanced", "Passthrough"]);
    await expect(page.getByRole("tablist", { name: "Hardware" })).toHaveAttribute("aria-orientation", "vertical");
    await expect(selected(page)).toHaveText("CPU and memory");
    await expect(page.getByRole("tabpanel")).toBeVisible();
    await expect(page.locator("#hw-cpu")).toBeVisible();
    await expect(page.locator("#hw-disks")).toHaveCount(0); // only the selected sub page is rendered
    await openVM(page, vm, "hardware", "nonsense");
    await expect(selected(page)).toHaveText("CPU and memory");
    await expect(page.locator("#hw-cpu")).toBeVisible();
  });

  test("a click shows that sub page only and puts it in the URL; reload and back keep it", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await subPage(page, "Disks");
    await expect(page).toHaveURL(/tab=hardware&sec=disks/);
    await expect(page.locator("#hw-disks")).toBeVisible();
    await expect(page.locator("#hw-cdrom")).toBeVisible(); // Disks and CD-ROM share a sub page
    await expect(page.locator("#hw-cpu")).toHaveCount(0);
    await page.reload();
    await expect(selected(page)).toHaveText("Disks");
    await expect(page.locator("#hw-disks")).toBeVisible();
    await subPage(page, "Boot and firmware");
    await expect(page.locator("#hw-boot")).toBeVisible();
    await expect(page.locator("#hw-firmware")).toBeVisible();
    await page.goBack();
    await expect(selected(page)).toHaveText("Disks");
    await expect(page.locator("#hw-disks")).toBeVisible();
    // another tab and back starts at the first sub page again
    await page.locator(".tabs").getByRole("button", { name: "Overview" }).click();
    await page.locator(".tabs").getByRole("button", { name: "Hardware" }).click();
    await expect(selected(page)).toHaveText("CPU and memory");
  });

  test("the list keeps its place and size when the sub page changes, and stays in view", async ({ page }) => {
    await openVM(page, vm, "hardware");
    const box = async () => page.locator(".subnav").boundingBox();
    const before = await box();
    await subPage(page, "Network interfaces");
    const after = await box();
    expect(after).toEqual(before);
    expect(await page.locator(".subnav").evaluate((e) => getComputedStyle(e).position)).toBe("sticky");
    const w = before!.width;
    expect(w).toBeGreaterThanOrEqual(160);
    expect(w).toBeLessThanOrEqual(200);
  });

  test("arrow keys move between the sub pages", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await selected(page).focus();
    await page.keyboard.press("ArrowDown");
    await expect(selected(page)).toHaveText("Disks");
    await expect(selected(page)).toBeFocused();
    await expect(page).toHaveURL(/sec=disks/);
    await page.keyboard.press("ArrowDown");
    await expect(selected(page)).toHaveText("Network interfaces");
    await page.keyboard.press("ArrowUp");
    await expect(selected(page)).toHaveText("Disks");
    await page.keyboard.press("End");
    await expect(selected(page)).toHaveText("Passthrough");
    await page.keyboard.press("ArrowDown");
    await expect(selected(page)).toHaveText("CPU and memory"); // wraps round
    await page.keyboard.press("ArrowUp");
    await expect(selected(page)).toHaveText("Passthrough");
    await page.keyboard.press("Home");
    await expect(selected(page)).toHaveText("CPU and memory");
    await expect(page.locator("#hw-cpu")).toBeVisible();
  });

  test("Snapshots and Backups have Snapshots or Backups, and Schedule", async ({ page }) => {
    await openVM(page, vm, "snapshots");
    await expect(entries(page)).toHaveText(["Snapshots", "Schedule"]);
    await expect(page.getByRole("button", { name: "Take snapshot" })).toBeVisible();
    await expect(page.locator("#snap-schedule")).toHaveCount(0);
    await subPage(page, "Schedule");
    await expect(page).toHaveURL(/tab=snapshots&sec=schedule/);
    await expect(page.locator("#snap-schedule")).toBeVisible();
    await expect(page.getByRole("button", { name: "Take snapshot" })).toHaveCount(0);
    await openVM(page, vm, "backups");
    await expect(entries(page)).toHaveText(["Backups", "Schedule"]);
    await expect(page.getByRole("button", { name: "Back up now" })).toBeVisible();
    await expect(page.locator("#backup-schedule")).toHaveCount(0);
    await subPage(page, "Schedule");
    await expect(page).toHaveURL(/tab=backups&sec=schedule/);
    await expect(page.locator("#backup-schedule")).toBeVisible();
    // the tabs without sub pages have no column
    for (const t of ["Overview", "Performance"]) {
      await page.locator(".tabs").getByRole("button", { name: t, exact: true }).click();
      await expect(page.locator(".subnav")).toHaveCount(0);
    }
  });

  test("a link from the Actions menu opens the sub page that holds the card", async ({ page }) => {
    await openVM(page, vm);
    await vmAction(page, "Snapshots", "Schedule");
    await expect(page).toHaveURL(/tab=snapshots&sec=schedule/);
    await expect(selected(page)).toHaveText("Schedule");
    await expect(page.locator("#snap-schedule")).toBeInViewport();
    // from the Schedule sub page, Manage snapshots goes to the first sub page
    await vmAction(page, "Snapshots", "Manage snapshots");
    await expect(selected(page)).toHaveText("Snapshots");
    await expect(page).not.toHaveURL(/sec=schedule/);
  });

  test("node Settings has Connection and Encrypted migration", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/settings`);
    await expect(entries(page)).toHaveText(["Connection", "Encrypted migration"]);
    await expect(selected(page)).toHaveText("Connection");
    await expect(page.getByRole("heading", { name: "Connection", exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Encrypted migration", exact: true })).toHaveCount(0);
    await subPage(page, "Encrypted migration");
    await expect(page).toHaveURL(/\/node\/\d+\/settings\?sec=migration/);
    await expect(page.getByRole("heading", { name: "Encrypted migration", exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Connection", exact: true })).toHaveCount(0);
    await page.reload();
    await expect(selected(page)).toHaveText("Encrypted migration");
    // the dashboards and the other tabs have no column
    for (const t of ["summary", "performance", "vms", "storage", "network", "updates"]) {
      await page.goto(`/node/${n1}/${t}`);
      await expect(page.locator("main .tabs")).toBeVisible();
      await expect(page.locator(".subnav")).toHaveCount(0);
    }
  });

  test("on a phone the entries are one wrapping row above the page and nothing scrolls sideways", async ({ page, n1 }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const sideways = () => page.evaluate(() => Math.max(document.documentElement.scrollWidth - innerWidth, document.querySelector(".content-scroll")!.scrollWidth - document.querySelector(".content-scroll")!.clientWidth));
    for (const sec of ["cpu", "disks", "nics", "boot", "advanced", "passthrough"]) {
      await openVM(page, vm, "hardware", sec);
      await expect(selected(page)).toBeVisible();
      const nav = (await page.locator(".subnav").boundingBox())!;
      const body = (await page.locator(".subpage").boundingBox())!;
      expect(nav.y + nav.height, sec).toBeLessThanOrEqual(body.y + 1); // above the page, not beside it
      expect(nav.width).toBeGreaterThan(300);
      expect(await sideways(), sec).toBeLessThanOrEqual(1);
    }
    for (const [tab, sec] of [["snapshots", "schedule"], ["backups", "schedule"]]) {
      await openVM(page, vm, tab, sec);
      await expect(selected(page)).toBeVisible();
      expect(await sideways(), tab).toBeLessThanOrEqual(1);
    }
    await page.goto(`/node/${n1}/settings?sec=migration`);
    await expect(selected(page)).toHaveText("Encrypted migration");
    expect(await sideways()).toBeLessThanOrEqual(1);
    // every entry is reachable: they wrap instead of scrolling
    await openVM(page, vm, "hardware");
    const ys = await entries(page).evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().top)));
    expect(ys.length).toBe(6);
    expect(new Set(ys).size).toBeGreaterThan(1); // more than one row
    for (const e of await entries(page).all()) await expect(e).toBeInViewport({ ratio: 0.99 });
  });

  test("at 1280 x 720 the main area does not scroll on CPU and memory, nor on Boot and firmware", async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 720 });
    for (const sec of ["cpu", "boot"]) {
      await openVM(page, vm, "hardware", sec);
      await expect(page.locator(sec === "cpu" ? "#hw-cpu" : "#hw-firmware")).toBeVisible();
      await page.waitForLoadState("networkidle");
      const m = await page.evaluate(() => {
        const c = document.querySelector(".content-scroll")!;
        return { scroll: c.scrollHeight, client: c.clientHeight };
      });
      expect(m.scroll, `${sec}: the main area scrolls (${m.scroll} > ${m.client})`).toBeLessThanOrEqual(m.client);
    }
  });
});
