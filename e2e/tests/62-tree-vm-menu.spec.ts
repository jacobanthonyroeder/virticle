// The tree's right-click menu of a VM is the VM page's Actions menu (one definition, web/src/vmMenu.ts), plus Open and Console first.
import { Locator, Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog } from "../lib/ui";

const NAME = `${P}menu1`;

interface Entry {
  label: string;
  disabled: boolean;
  title: string;
  danger: boolean;
  items?: (Entry | "sep")[];
}

/** Reads a menu panel with every enabled submenu opened in turn: label, disabled state, reason, danger mark, order, separators. */
async function readPanel(panel: Locator): Promise<(Entry | "sep")[]> {
  const kids = panel.locator(":scope > *");
  const out: (Entry | "sep")[] = [];
  for (let i = 0; i < (await kids.count()); i++) {
    const k = kids.nth(i);
    if ((await k.getAttribute("role")) === "separator") {
      out.push("sep");
      continue;
    }
    const b = k.locator(":scope > button");
    const e: Entry = {
      label: (await b.locator(".lbl").innerText()).trim(),
      disabled: await b.isDisabled(),
      title: (await b.getAttribute("title")) ?? "",
      danger: ((await b.getAttribute("class")) ?? "").includes("danger"),
    };
    if ((await b.getAttribute("aria-haspopup")) === "menu" && !e.disabled) {
      await b.click();
      e.items = await readPanel(k.locator(":scope > .ctxmenu"));
      await b.click();
    }
    out.push(e);
  }
  return out;
}
const openMenu = (page: Page) => page.locator(".menu-pos > .ctxmenu").first();

async function pageMenu(page: Page) {
  await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
  const m = await readPanel(openMenu(page));
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);
  return m;
}
/** The tree's menu without its own first entries (Open, Console, separator). The VM detail arrives after the menu is shown: read again until no entry waits for it. */
async function treeMenu(page: Page, name: string) {
  const row = page.getByRole("tree", { name: "Resources" }).getByRole("treeitem", { name: new RegExp(`^${name}, `) });
  let m: (Entry | "sep")[] = [];
  for (let i = 0; i < 30; i++) {
    await row.click({ button: "right" });
    await expect(page.getByRole("menu").first()).toBeVisible();
    m = await readPanel(openMenu(page));
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu")).toHaveCount(0);
    if (!JSON.stringify(m).includes("Checking")) break;
    await page.waitForTimeout(300);
  }
  const head = m.slice(0, 3).map((e) => (e === "sep" ? "sep" : e.label));
  expect(head).toEqual(["Open", "Console", "sep"]);
  return m.slice(3);
}

test.describe.serial("the tree's VM menu is the Actions menu", () => {
  let vm: { node: number; uuid: string; name: string };
  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME, { start: true });
  });
  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  test("same entries, order, disabled states and reasons for a running VM", async ({ page, api }) => {
    await api.waitFor("running", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "running", 30_000, 500);
    await openVM(page, vm);
    const onPage = await pageMenu(page);
    expect(onPage.length).toBeGreaterThan(5);
    expect(await treeMenu(page, NAME)).toEqual(onPage);
  });

  test("the same for a shut-off VM", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("shut off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff", 30_000, 500);
    await openVM(page, vm);
    const onPage = await pageMenu(page);
    expect(await treeMenu(page, NAME)).toEqual(onPage);
  });

  test("Console in the tree opens the VM page with the console", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    await api.waitFor("running", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "running", 30_000, 500);
    await page.goto("/datacenter/vms");
    await page.getByRole("tree", { name: "Resources" }).getByRole("treeitem", { name: new RegExp(`^${NAME}, `) }).click({ button: "right" });
    await page.getByRole("menuitem", { name: "Console", exact: true }).click();
    await expect(page).toHaveURL(new RegExp(`/vm/${vm.node}/${vm.uuid}\\?tab=console`));
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("shut off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff", 30_000, 500);
  });

  test("entries that need a dialog go to the page and open it; the address loses ?do", async ({ page }) => {
    const row = () => page.getByRole("tree", { name: "Resources" }).getByRole("treeitem", { name: new RegExp(`^${NAME}, `) });
    const pick = async (...path: string[]) => {
      await page.goto("/datacenter/vms");
      await row().click({ button: "right" });
      for (const p of path.slice(0, -1)) await page.getByRole("menuitem", { name: p, exact: true }).click();
      const last = page.getByRole("menuitem", { name: path[path.length - 1], exact: true });
      await expect(last).toBeEnabled();
      await last.click();
    };
    await pick("Clone");
    await expect(dialog(page, `Clone ${NAME}`)).toBeVisible();
    await expect(page).not.toHaveURL(/do=/);
    await page.keyboard.press("Escape");
    await pick("Snapshots", "Take snapshot");
    await expect(dialog(page, "Take snapshot")).toBeVisible();
    await expect(page).not.toHaveURL(/do=/);
    await page.keyboard.press("Escape");
    await pick("Edit Settings", "Boot order");
    await expect(dialog(page, "Boot order")).toBeVisible();
    await page.keyboard.press("Escape");
    await pick("Migrate", "Migrate to another node");
    await expect(dialog(page, new RegExp(`Migrate ${NAME}`))).toBeVisible();
    await page.keyboard.press("Escape");
    await pick("Migrate", "Move the disk to another pool");
    await expect(dialog(page, /^Move /)).toBeVisible();
    await page.keyboard.press("Escape");
    await pick("Snapshots", "Schedule");
    await expect(page).toHaveURL(/tab=snapshots/);
    await expect(page).not.toHaveURL(/do=/);
    await pick("More", "Edit XML");
    await expect(page).toHaveURL(/tab=xml/);
    await pick("More", "Delete VM");
    await expect(dialog(page, `Delete ${NAME}?`)).toBeVisible();
    await page.keyboard.press("Escape");
  });

  test("power actions run from the tree; a template has the same menu there as on its page; Convert back runs in place", async ({ page, api }) => {
    await page.goto("/datacenter/vms");
    const tree = page.getByRole("tree", { name: "Resources" });
    await tree.getByRole("treeitem", { name: new RegExp(`^${NAME}, `) }).click({ button: "right" });
    await page.getByRole("menuitem", { name: "Power", exact: true }).click();
    await page.getByRole("menuitem", { name: "Power On", exact: true }).click();
    await api.waitFor("running", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "running", 30_000, 500);
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("shut off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff", 30_000, 500);
    await api.put(`/nodes/${vm.node}/vms/${vm.uuid}/template`, { template: true });
    await openVM(page, vm);
    const group = tree.locator(`[data-key="t:${vm.node}"]`);
    if ((await group.getAttribute("aria-expanded")) !== "true") await group.locator(`button[aria-label="Expand"]`).click();
    const onPage = await pageMenu(page);
    const t = await treeMenu(page, NAME);
    expect(t).toEqual(onPage);
    const find = (m: (Entry | "sep")[], l: string) => m.find((e): e is Entry => e !== "sep" && e.label === l)!;
    expect(find(t, "Convert back to a VM").disabled).toBe(false);
    expect(find(t, "Replication").disabled).toBe(true);
    await tree.getByRole("treeitem", { name: new RegExp(`^${NAME}, `) }).click({ button: "right" });
    await page.getByRole("menuitem", { name: "Convert back to a VM", exact: true }).click();
    await api.waitFor("a VM again", async () => !(await api.vmDetail(vm.node, vm.uuid)).template, 30_000, 500);
  });
});
