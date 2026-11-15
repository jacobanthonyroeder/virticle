// Backups tab: back up now, restore as a new VM, export download, delete; global Backups page.
import { cleanVMs, createCirros, expect, openVM, stopTask, tab, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, dockRow, expectBadge, row } from "../lib/ui";
import { readFileSync, statSync } from "fs";

const NAME = `${P}bak1`;
const RESTORED = `${P}bak1-restored`;

test.describe.serial("backups", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, RESTORED, NAME));

  test("back up now (VM shut off) and see it in the list", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    vm = await createCirros(api, n1, NAME);
    await openVM(page, vm, "backups");
    await expect(page.getByText("No backups", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Back up now" }).first().click();
    const dlg = dialog(page, `Back up ${NAME}`);
    await expect(dlg.getByRole("combobox", { name: "Target pool" })).toContainText("default");
    await dlg.getByRole("button", { name: "Back up now" }).click();
    // the dialog closes once the server accepted the backup; the Tasks dock follows it
    await expect(dlg).toBeHidden();
    await expect(dockRow(page, "backup", NAME).first()).toContainText("Ok", { timeout: 200_000 });
    const r = page.locator("tbody tr").first();
    await expect(r).toContainText("default");
    await expect(r).toContainText("vda");
    const list = await api.get(`/backups?uuid=${vm.uuid}`);
    expect(list).toHaveLength(1);
    expect(list[0].manifest.vm).toBe(NAME);
    expect(list[0].size).toBeGreaterThan(1024 * 1024);
  });

  test("the global Backups page lists it too", async ({ page }) => {
    await page.goto("/backups");
    await expect(page.getByRole("heading", { name: "Backups" }).first()).toBeVisible();
    await expect(row(page, NAME).first()).toBeVisible({ timeout: 60_000 });
  });

  test("export downloads a tar file", async ({ page }) => {
    await openVM(page, vm, "backups");
    const dl = page.waitForEvent("download");
    await page.getByRole("link", { name: "Export" }).click();
    const d = await dl;
    expect(d.suggestedFilename()).toMatch(/\.tar$/);
    const path = await d.path();
    expect(statSync(path!).size).toBeGreaterThan(1024 * 1024);
    // a tar archive carries "ustar" at offset 257 of its first header
    expect(readFileSync(path!).subarray(257, 262).toString()).toBe("ustar");
  });

  test("restore as a new VM", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Restore" }).click();
    const dlg = dialog(page, `Restore ${NAME}`);
    await expect(dlg.getByLabel("New VM name")).toHaveValue(RESTORED);
    await expect(dlg.getByLabel("Start the VM afterwards")).toBeChecked(); // on by default
    await dlg.getByLabel("Start the VM afterwards").uncheck(); // the checks below expect it shut off
    await dlg.getByRole("button", { name: "Restore", exact: true }).click();
    await expect(dlg).toBeHidden();
    await expect(dockRow(page, "restore", RESTORED).first()).toContainText("Ok", { timeout: 200_000 });
    const got = await api.vmByName(RESTORED);
    expect(got).toBeTruthy();
    expect(got.uuid).not.toBe(vm.uuid);
    const d = await api.vmDetail(got.node, got.uuid);
    expect(d.nics[0].mac).not.toBe((await api.vmDetail(vm.node, vm.uuid)).nics[0].mac);
    await page.goto("/datacenter/vms");
    await expectBadge(row(page, RESTORED), "shutoff");
  });

  test("delete the backup", async ({ page, api }) => {
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Delete" }).click();
    const dlg = dialog(page, "Delete backup");
    await dlg.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(dlg).toBeHidden();
    await expect(page.getByText("No backups", { exact: true })).toBeVisible();
    expect(await api.get(`/backups?uuid=${vm.uuid}`)).toHaveLength(0);
  });
});

// Stopping a running task (POST /api/tasks/{id}/cancel) ends the work on the node and leaves nothing half-made. The Debian
// image makes the VM big enough (about 1 GiB of backup) to be stopped in the middle.
test.describe.serial("stopping restore, export and clone", () => {
  const SRC = `${P}bak2`;
  const RS = `${P}bak2-restored`;
  const CL = `${P}bak2-clone`;
  let vm: { node: number; uuid: string; name: string };
  let dir: string;

  const poolVolumes = async (api: any, n: number): Promise<string[]> => (await api.get(`/nodes/${n}/pools/default/volumes`)).map((v: any) => v.name);
  const backups = (api: any) => api.get(`/backups?uuid=${vm.uuid}`) as Promise<any[]>;

  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, CL, RS, SRC);
    for (const b of (await api.get("/backups").catch(() => [])) as any[]) {
      if ((b.manifest?.vm ?? "").startsWith(SRC)) await api.del(`/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}`).catch(() => {});
    }
  });

  test("a backup of a Debian VM to work on", async ({ api, n1 }) => {
    test.setTimeout(240_000);
    const r = await api.post(`/nodes/${n1}/vms`, { name: SRC, vcpus: 1, memoryMiB: 128, firmware: "bios", pool: "default", diskGiB: 3, baseVolume: env.debian, network: "default", start: false });
    vm = { node: n1, uuid: r.uuid, name: SRC };
    await api.post(`/nodes/${n1}/vms/${vm.uuid}/backup`, { pool: "default" });
    const b = await api.waitFor("the backup", async () => (await backups(api))[0], 200_000, 2000);
    dir = b.dir;
  });

  test("stopping a restore removes the work directory and the new volumes", async ({ api, n1 }) => {
    test.setTimeout(240_000);
    const restore = () => api.post(`/nodes/${n1}/backups/default/${encodeURIComponent(dir)}/restore`, { mode: "new", name: RS, pool: "default", start: false });
    const { seconds } = await stopTask(api, "restore", restore, 3000);
    expect(seconds).toBeLessThan(10);
    expect(await api.vmByName(RS)).toBeUndefined();
    expect(await poolVolumes(api, n1)).not.toContain(`${RS}.qcow2`);
    // The backup was only read: it restores for real afterwards.
    await restore();
    await api.waitFor("the restored VM", async () => api.vmByName(RS), 120_000, 2000);
    await cleanVMs(api, RS);
  });

  test("stopping an export ends the stream", async ({ api, n1 }) => {
    const { seconds } = await stopTask(api, "backup export", () => api.ctx.get(`/api/nodes/${n1}/backups/default/${encodeURIComponent(dir)}/export`), 1500);
    expect(seconds).toBeLessThan(10);
    expect(await backups(api)).toHaveLength(1);
  });

  test("stopping a full clone removes the half-made copy", async ({ api, n1 }) => {
    test.setTimeout(240_000);
    const clone = () => api.post(`/nodes/${n1}/vms/${vm.uuid}/clone`, { name: CL });
    const { seconds } = await stopTask(api, "clone", clone, 500);
    expect(seconds).toBeLessThan(10);
    expect(await api.vmByName(CL)).toBeUndefined();
    expect(await poolVolumes(api, n1)).not.toContain(`${CL}.qcow2`);
    // The source is untouched: a clone still works.
    await clone();
    expect(await api.vmByName(CL)).toBeTruthy();
    await cleanVMs(api, CL);
  });
});
