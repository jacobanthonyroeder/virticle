// VM page polish: Edit Settings dialogs that share state with the Hardware tab, the Power submenu order, the
// the plain Take snapshot dialog, the aligned Hardware tab with collapsed sections, the pool picker, the autostart box.
import { cleanVMs, createCirros, expect, openVM, subPage, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, expectBadge, field } from "../lib/ui";
import { poolLevel } from "../../web/src/poolLevel";

const NAME = `${P}pp1`;
const POOL = `${P}pool24`;

const AREAS = ["CPU and memory", "Disks", "Network interfaces", "CD-ROM", "Boot order", "Firmware and TPM", "Passthrough devices"];

test.describe("pool level (pure)", () => {
  test("below warn, between, at and above critical, zero capacity", () => {
    expect(poolLevel(10, 100)).toEqual({ pct: 10, level: "ok" });
    expect(poolLevel(84.9, 100).level).toBe("ok");
    expect(poolLevel(85, 100).level).toBe("warn");
    expect(poolLevel(94, 100).level).toBe("warn");
    expect(poolLevel(95, 100).level).toBe("crit");
    expect(poolLevel(100, 100).level).toBe("crit");
    expect(poolLevel(120, 100).pct).toBe(100);
    expect(poolLevel(50, 100, 40, 60).level).toBe("warn");
    expect(poolLevel(0, 0)).toEqual({ pct: 0, level: "ok" });
    expect(poolLevel(5, 0)).toEqual({ pct: 0, level: "ok" });
  });
});

test.describe.serial("VM page polish", () => {
  let vm: { node: number; uuid: string; name: string };
  const actions = (page: any) => page.locator(".head").getByRole("button", { name: "Actions", exact: true });
  const item = (page: any, name: string) => page.getByRole("menuitem", { name, exact: true });
  const edit = async (page: any, entry: string) => {
    await actions(page).click();
    await item(page, "Edit Settings").click();
    await item(page, entry).click();
  };
  const card = (page: any, title: string) => page.locator("section.card").filter({ has: page.getByRole("heading", { name: title, exact: true }) });

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
    await api.post(`/nodes/${n1}/pools`, { name: POOL, type: "dir", target: `/var/lib/libvirt/${POOL}` });
  });
  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME);
    for (const v of await api.get(`/nodes/${n1}/pools/${POOL}/volumes`).catch(() => [])) await api.del(`/nodes/${n1}/pools/${POOL}/volumes/${encodeURIComponent(v.name)}`).catch(() => {});
    await api.del(`/nodes/${n1}/pools/default/volumes/${NAME}-disk1.qcow2`).catch(() => {});
    await api.post(`/nodes/${n1}/pools/${POOL}/action`, { action: "remove" }).catch(() => {});
  });

  test("each Edit Settings entry opens a dialog over a dimmed backdrop; URL and tab stay", async ({ page }) => {
    await openVM(page, vm);
    const url = page.url();
    for (const area of AREAS) {
      await edit(page, area);
      const dlg = dialog(page, area);
      await expect(dlg).toBeVisible();
      const bg = await page.locator(".backdrop").first().evaluate((e) => getComputedStyle(e).backgroundColor);
      expect(bg).not.toBe("rgba(0, 0, 0, 0)");
      expect(bg).not.toBe("transparent");
      expect(page.url()).toBe(url);
      await expect(page.locator(".tabs .on")).toHaveText("Overview");
      // the dialog fits the window and scrolls inside when tall
      const box = await dlg.boundingBox();
      const vp = page.viewportSize()!;
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
      await page.keyboard.press("Escape");
      await expect(dialog(page)).toHaveCount(0);
    }
  });

  test("memory changed in the dialog shows on the Hardware tab behind it and on the overview, and the other way round", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    await edit(page, "CPU and memory");
    const dlg = dialog(page, "CPU and memory");
    await field(dlg, "Memory (MiB)").fill("192");
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await field(dlg, "Maximum memory (MiB)").fill("256");
    await dlg.getByRole("button", { name: "Apply" }).click();
    await expect(dlg).toContainText("Saved.");
    await expect(card(page, "CPU and memory")).toContainText("192 MiB"); // the tab, behind the dialog, without a reload
    await page.keyboard.press("Escape");
    expect((await api.vmDetail(vm.node, vm.uuid)).currentMemoryKiB).toBe(192 * 1024);
    await page.locator(".tabs").getByRole("button", { name: "Overview" }).click();
    const ov = card(page, "Hardware");
    await expect(ov.locator("dt", { hasText: "Memory" }).locator("+ dd")).toHaveText("256 MiB");
    // the other way: change on the tab, reopen the dialog
    await page.locator(".tabs").getByRole("button", { name: "Hardware" }).click();
    const c = card(page, "CPU and memory");
    await c.getByRole("button", { name: "Edit" }).click();
    await field(c, "Memory (MiB)").fill("224");
    await c.getByRole("button", { name: "Apply" }).click();
    await expect(c).toContainText("224 MiB");
    await edit(page, "CPU and memory");
    await expect(field(dialog(page, "CPU and memory"), "Memory (MiB)")).toHaveValue("224");
    await page.keyboard.press("Escape");
  });

  test("Disks dialog: nested Add disk, Escape closes only the top dialog, the new disk shows in both", async ({ page, api, n1 }) => {
    await openVM(page, vm, "hardware", "disks");
    await edit(page, "Disks");
    const disks = dialog(page, "Disks");
    await disks.getByRole("button", { name: "Add disk" }).click();
    await expect(dialog(page, "Add disk")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page, "Add disk")).toHaveCount(0);
    await expect(disks).toBeVisible();
    await disks.getByRole("button", { name: "Add disk" }).click();
    const add = dialog(page, "Add disk");
    await field(add, "Size (GiB)").fill("1");
    await add.getByRole("button", { name: "Add disk" }).click();
    await expect(add).toBeHidden();
    // the volume name is generated
    await expect(disks.locator("tbody tr", { hasText: `${NAME}-disk1.qcow2` })).toBeVisible();
    await expect(card(page, "Disks").locator("tbody tr", { hasText: `${NAME}-disk1.qcow2` })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
    expect((await api.vmDetail(vm.node, vm.uuid)).disks.some((k: any) => k.source?.endsWith(`${NAME}-disk1.qcow2`))).toBe(true);
  });

  test("pool picker: percent, used of size and a bar per pool; keyboard choice is what the request sends", async ({ page }) => {
    await openVM(page, vm, "hardware", "disks");
    await page.getByRole("button", { name: "Add disk" }).click();
    const dlg = dialog(page, "Add disk");
    const combo = dlg.getByRole("combobox", { name: "Storage pool" });
    await expect(combo).toContainText(/\d+% used/);
    await expect(combo).toContainText(/ used of /);
    await expect(combo.locator(".ps-bar")).toBeVisible();
    await combo.click();
    const list = page.getByRole("listbox");
    const opts = list.getByRole("option");
    expect(await opts.count()).toBeGreaterThanOrEqual(2);
    for (let i = 0; i < (await opts.count()); i++) {
      const o = opts.nth(i);
      await expect(o).toContainText(/\d+% used|inactive/);
      await expect(o).toContainText(/ used of /);
      await expect(o.locator(".ps-bar")).toHaveCount(1);
    }
    // the list stays inside the window
    const box = await list.boundingBox();
    const vp = page.viewportSize()!;
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
    expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height);
    // Escape closes the list only
    await page.keyboard.press("Escape");
    await expect(list).toHaveCount(0);
    await expect(dlg).toBeVisible();
    // keyboard: open, type-ahead to the new pool, Enter
    await combo.focus();
    await page.keyboard.press("ArrowDown");
    await expect(page.getByRole("listbox")).toBeVisible();
    await page.keyboard.type(POOL);
    await expect(page.getByRole("option", { name: new RegExp(POOL) })).toHaveClass(/active/);
    await page.keyboard.press("Enter");
    await expect(page.getByRole("listbox")).toHaveCount(0);
    await expect(combo).toContainText(POOL);
    // a size no pool has room for is called out, and the pool stays selectable
    await field(dlg, "Size (GiB)").fill("60000");
    await expect(combo).toContainText("not enough free space");
    await field(dlg, "Size (GiB)").fill("1");
    await expect(combo).not.toContainText("not enough free space");
    const req = page.waitForRequest((r) => r.method() === "POST" && /\/disks$/.test(r.url()));
    await dlg.getByRole("button", { name: "Add disk" }).click();
    expect((await req).postDataJSON().pool).toBe(POOL);
    await expect(dlg).toBeHidden();
    await expect(card(page, "Disks").locator("tbody tr", { hasText: `${POOL}/${NAME}-disk1.qcow2` })).toBeVisible();
  });

  test("the Power submenu has its entries and separators in the requested order", async ({ page }) => {
    await openVM(page, vm);
    await actions(page).click();
    const parent = item(page, "Power");
    await parent.click();
    const sub = page.locator(".mi", { has: parent }).getByRole("menu");
    const got = await sub.locator(":scope > *").evaluateAll((els) => els.map((e) => (e.getAttribute("role") === "separator" ? "-" : (e.textContent ?? "").trim())));
    expect(got).toEqual(["Power On", "Reboot (Graceful)", "Shutdown (graceful)", "-", "Pause", "Resume", "-", "Halt (Force Shutdown)", "Reset (Hard Reboot)", "-", "Sync guest clock"]);
    expect(got.join("|")).not.toContain("force off after");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
  });

  test("autostart: the box follows the click at once, no Yes or No text", async ({ page, api }) => {
    await openVM(page, vm);
    const url = page.url();
    const box = page.getByRole("checkbox", { name: "Start on host boot" });
    await expect(box).not.toBeChecked();
    await box.click();
    await expect(box).toBeChecked();
    await expect.poll(async () => (await api.vmDetail(vm.node, vm.uuid)).autostart).toBe(true);
    expect(page.url()).toBe(url);
    const dd = page.locator("dt", { hasText: "Start on host boot" }).locator("+ dd");
    await expect(dd).not.toContainText(/Yes|No/);
    await box.click();
    await expect(box).not.toBeChecked();
    await expect.poll(async () => (await api.vmDetail(vm.node, vm.uuid)).autostart).toBe(false);
  });

  test("Hardware tab: one sub page at a time, cards do not overlap, edges line up, gaps are equal", async ({ page }) => {
    await page.setViewportSize({ width: 1360, height: 900 });
    await openVM(page, vm, "hardware");
    // the sub page column lists every area; only the selected sub page is in the page
    await expect(page.locator(".subnav").getByRole("tab")).toHaveText(["CPU and memory", "Disks", "Network interfaces", "Boot and firmware", "Advanced", "Passthrough"]);
    const cards = { "CPU and memory": ["hw-cpu"], Disks: ["hw-disks", "hw-cdrom"], "Network interfaces": ["hw-nics"], "Boot and firmware": ["hw-boot", "hw-firmware"], Advanced: ["hw-advanced"], Passthrough: ["hw-passthrough"] };
    for (const [label, ids] of Object.entries(cards)) {
      await subPage(page, label);
      await expect(page.locator(`#${ids[0]}`)).toBeVisible();
      const boxes = await page.locator(".hw > [id]").evaluateAll((els) => els.map((e) => ({ id: e.id, ...e.getBoundingClientRect().toJSON() })));
      expect(boxes.map((b) => b.id).sort(), label).toEqual([...ids].sort());
      for (let i = 0; i < boxes.length; i++)
        for (let j = i + 1; j < boxes.length; j++) {
          const a = boxes[i], b = boxes[j];
          const overlap = Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 && Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
          expect(overlap, `${a.id} overlaps ${b.id}`).toBe(false);
        }
      // the cards start at the top of the sub page column and end at the right edge of the page area
      const nav = await page.locator(".subnav").boundingBox();
      const page_ = await page.locator(".subpage").boundingBox();
      expect(Math.abs(Math.max(...boxes.map((b) => b.right)) - (page_!.x + page_!.width)), `${label} right edge`).toBeLessThan(2);
      expect(Math.abs(Math.min(...boxes.map((b) => b.top)) - nav!.y), `${label} top`).toBeLessThan(2);
      if (label === "Boot and firmware") {
        // side by side, the two cards end level: no hole under the shorter one
        expect(Math.abs(boxes[0].top - boxes[1].top)).toBeLessThan(2);
        expect(Math.abs(boxes[0].bottom - boxes[1].bottom)).toBeLessThan(2);
      } else {
        // one under the other, with the same gap everywhere
        const sorted = [...boxes].sort((a, b) => a.top - b.top);
        for (let i = 1; i < sorted.length; i++) expect(Math.abs(sorted[i].top - sorted[i - 1].bottom - 14), `${label} gap`).toBeLessThanOrEqual(1);
        for (const b of boxes) {
          expect(Math.abs(b.left - sorted[0].left)).toBeLessThan(2);
          expect(Math.abs(b.right - (page_!.x + page_!.width)), `${b.id} right edge`).toBeLessThan(2);
        }
      }
    }
    // no card spills out of the page area
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0);
  });

  test("Advanced and Passthrough are open at once on their own sub page, with no collapse header", async ({ page }) => {
    await openVM(page, vm, "hardware", "advanced");
    const adv = page.locator("#hw-advanced");
    await expect(adv).toContainText("vCPU pinning");
    await expect(adv.locator(".disc-head")).toHaveCount(0);
    await page.reload();
    await expect(adv).toContainText("vCPU pinning");
    await subPage(page, "Passthrough");
    const pt = page.locator("#hw-passthrough");
    await expect(pt).toContainText("No host devices");
    await expect(pt.locator(".disc-head")).toHaveCount(0);
  });

  test("the outcome line of a change goes away when another sub page is chosen", async ({ page }) => {
    await openVM(page, vm, "hardware", "cpu");
    const c = card(page, "CPU and memory");
    await c.getByRole("button", { name: "Edit" }).click();
    await field(c, "Memory (MiB)").fill("192");
    await c.getByRole("button", { name: "Apply" }).click();
    const outcome = page.getByText(/^(Applied and saved|Saved)\./);
    await expect(outcome).toBeVisible({ timeout: 30_000 });
    await subPage(page, "Disks");
    await expect(outcome).toHaveCount(0);
    await subPage(page, "CPU and memory");
    await expect(outcome).toHaveCount(0);
  });

  test("the Passthrough dialog shows its content without a collapsed header; Advanced is only a card on the tab", async ({ page }) => {
    await openVM(page, vm);
    await actions(page).click();
    await item(page, "Edit Settings").click();
    await expect(item(page, "Advanced")).toHaveCount(0);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await edit(page, "Passthrough devices");
    await expect(dialog(page, "Passthrough devices")).toContainText("No host devices");
    await page.keyboard.press("Escape");
  });

  test("Add CD-ROM drive stays available up to four drives", async ({ page, api }) => {
    for (let i = 0; i < 2; i++) await api.post(`${"/nodes/" + vm.node}/vms/${vm.uuid}/cdrom`);
    await openVM(page, vm, "hardware", "disks");
    const c = card(page, "CD-ROM");
    await expect(c.locator("tbody tr")).toHaveCount(2);
    await c.getByRole("button", { name: "Add CD-ROM drive" }).click();
    await expect(c.locator("tbody tr")).toHaveCount(3);
    await c.getByRole("button", { name: "Add CD-ROM drive" }).click();
    await expect(c.locator("tbody tr")).toHaveCount(4);
    await expect(c.getByRole("button", { name: "Add CD-ROM drive" })).toHaveCount(0);
  });

  test("the Take snapshot dialog has no type choice", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    await openVM(page, vm);
    await expectBadge(page.locator(".head"), "running");
    await actions(page).click();
    await item(page, "Snapshots").click();
    await item(page, "Take snapshot").click();
    const dlg = dialog(page, "Take snapshot");
    // no snapshot type to choose and no help tip about types: the type is always automatic
    await expect(dlg.getByRole("button", { name: "About snapshot types" })).toHaveCount(0);
    await expect(dlg.getByRole("combobox", { name: "Snapshot type" })).toHaveCount(0);
    await expect(dlg.getByLabel(/quiesce/i)).toHaveCount(0);
    await expect(dlg.getByLabel("Save the running state too")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
  });

  test("a graphics card cannot be attached while the VM runs, and the reason is shown", async ({ page }) => {
    // the VM is running (the previous test started it); the node has no GPU, so the listing is made up
    await page.route(/\/hostdevs$/, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const dev = (id: string, category: string) => ({ kind: "pci", id, nodedev: "pci_" + id, category, vendor: "Vendor", product: category, vendorId: "10de", productId: "1234", iommuGroup: 1, usedBy: [], unsafe: false });
      await route.fulfill({ json: { devices: [dev("0000:0a:00.0", "gpu"), dev("0000:0b:00.0", "storage")], attached: [], iommu: true, factsKnown: true, vm: vm.uuid, warning: "" } });
    });
    await openVM(page, vm, "hardware", "passthrough");
    await page.getByRole("button", { name: "Add host device" }).click();
    const dlg = dialog(page, "Host devices of this node");
    const gpu = dlg.locator("tr", { hasText: "0000:0a:00.0" });
    await expect(gpu.getByRole("button", { name: "Attach" })).toBeDisabled();
    await expect(gpu).toContainText("Shut the VM down to attach a graphics card");
    await expect(gpu.getByRole("button", { name: "Attach" })).toHaveAttribute("title", "Shut the VM down to attach a graphics card");
    await expect(dlg.locator("tr", { hasText: "0000:0b:00.0" }).getByRole("button", { name: "Attach" })).toBeEnabled();
    await page.keyboard.press("Escape");
  });
});
