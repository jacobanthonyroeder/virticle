// "UTC hardware clock" of Windows guests: a checkbox in New VM (Windows only, on by default) and in Edit Settings,
// Firmware and TPM, with a "Pending" state while the switch waits for the next shutdown from Virticle. Nothing here
// boots Windows: the switch at shutdown and the registry value were tried on a real Windows guest by hand.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field, pick, pickPool, vmAction } from "../lib/ui";

const ON = `${P}utc-on`;
const OFF = `${P}utc-off`;
const CL = `${P}utc-cl`;
const LIN = `${P}utc-lin`;

async function newVMDialog(page: any) {
  await page.goto("/datacenter/vms");
  await page.getByRole("button", { name: "New VM" }).click();
  return dialog(page, "New virtual machine");
}

/** Fills the New VM dialog for a Windows VM on an empty disk that is not started; the UTC box as `utc`. */
async function createWindows(page: any, name: string, utc: boolean) {
  const dlg = await newVMDialog(page);
  await field(dlg, "Name").fill(name);
  if (await field(dlg, "Node").count()) await pick(dlg, field(dlg, "Node"), env.node1);
  await pick(dlg, "Guest OS type", "Windows");
  const box = dlg.getByRole("checkbox", { name: "UTC hardware clock" });
  await expect(box).toBeChecked();
  if (!utc) await box.uncheck();
  await dlg.getByRole("button", { name: "Empty disk" }).click();
  if (await dlg.getByRole("combobox", { name: "Storage pool" }).count()) await pickPool(dlg, "Storage pool", "default");
  await field(dlg, "Disk size (GiB)").fill("1");
  await dlg.getByRole("checkbox", { name: "Start after creating" }).uncheck();
  await dlg.getByRole("button", { name: "Create VM" }).click();
  await expect(dlg).toBeHidden();
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
}

test.describe.serial("UTC hardware clock", () => {
  const vm = async (api: any, name: string) => {
    const v = await api.vmByName(name);
    return { node: v.node as number, uuid: v.uuid as string, name };
  };
  const clock = async (api: any, v: { node: number; uuid: string }) => api.get(`/nodes/${v.node}/vms/${v.uuid}/clock`);

  test.afterAll(async ({ api }) => cleanVMs(api, ON, OFF, CL, LIN));

  test("New VM: the box is there for Windows only, on by default", async ({ page }) => {
    const dlg = await newVMDialog(page);
    await expect(dlg.getByRole("checkbox", { name: "UTC hardware clock" })).toHaveCount(0);
    await pick(dlg, "Guest OS type", "Windows");
    await expect(dlg.getByRole("checkbox", { name: "UTC hardware clock" })).toBeChecked();
    await expect(dlg.getByRole("button", { name: "About UTC hardware clock" })).toBeVisible();
    await pick(dlg, "Guest OS type", "Linux / other");
    await expect(dlg.getByRole("checkbox", { name: "UTC hardware clock" })).toHaveCount(0);
    await page.keyboard.press("Escape");
  });

  test("on: local time now, the switch to UTC waits for the next shutdown", async ({ page, api }) => {
    await createWindows(page, ON, true);
    const v = await vm(api, ON);
    const c = await clock(api, v);
    expect(c).toMatchObject({ windows: true, utc: true, pending: "utc", saved: "localtime" });
    expect((await api.vmDetail(v.node, v.uuid)).xml).toContain("utcclock");
  });

  test("off: a local-time VM without the mark", async ({ page, api }) => {
    await createWindows(page, OFF, false);
    const v = await vm(api, OFF);
    const c = await clock(api, v);
    expect(c).toMatchObject({ windows: true, utc: false, saved: "localtime" });
    expect(c.pending).toBeUndefined();
    expect((await api.vmDetail(v.node, v.uuid)).xml).not.toContain("utcclock");
  });

  test("an old client that sends no field gets the switch", async ({ api, n1 }) => {
    const r = await api.post(`/nodes/${n1}/vms`, { name: CL, vcpus: 1, memoryMiB: 256, osType: "windows", pool: "default", diskGiB: 1 });
    expect(await clock(api, { node: n1, uuid: r.uuid })).toMatchObject({ utc: true, pending: "utc" });
    await cleanVMs(api, CL);
  });

  test("Edit Settings, Firmware and TPM: shows the state and changes it", async ({ page, api }) => {
    const v = await vm(api, ON);
    await openVM(page, v);
    await vmAction(page, "Edit Settings", "Firmware and TPM");
    const dlg = dialog(page, "Firmware and TPM");
    const box = dlg.getByRole("checkbox", { name: "UTC", exact: true });
    await expect(box).toBeChecked();
    await expect(dlg.locator(".badge.s-pending")).toHaveText("Pending");
    await box.click(); // the box follows the server: it changes when the answer is back
    await expect(box).not.toBeChecked();
    await expect(dlg.locator(".badge.s-pending")).toHaveCount(0);
    await expect(dlg).toContainText("UTC hardware clock off.");
    expect(await clock(api, v)).toMatchObject({ utc: false, saved: "localtime" });
    await box.click();
    await expect(box).toBeChecked();
    await expect(dlg.locator(".badge.s-pending")).toHaveText("Pending");
    await expect(dlg).toContainText("Changes over at the next Shut down from Virticle while the guest agent runs.");
    expect(await clock(api, v)).toMatchObject({ utc: true, pending: "utc" });
    // nothing is cut off at 1280 px
    const overflow = await dlg.evaluate((el: HTMLElement) => el.scrollWidth > el.clientWidth);
    expect(overflow).toBe(false);
    await page.keyboard.press("Escape");
  });

  test("a clone inherits the waiting switch; Guest setup sees a Windows guest", async ({ api }) => {
    const v = await vm(api, ON);
    const info = await api.get(`/nodes/${v.node}/vms/${v.uuid}/clone-info`);
    expect(info.guest).toBe("windows");
    const r = await api.post(`/nodes/${v.node}/vms/${v.uuid}/clone`, { name: CL, start: false });
    expect(r.guest).toBe("windows");
    expect(await clock(api, { node: v.node, uuid: r.uuid })).toMatchObject({ utc: true, pending: "utc" });
  });

  test("a Linux VM has no Hardware clock row and the setting is refused", async ({ page, api, n1 }) => {
    const v = await createCirros(api, n1, LIN);
    await openVM(page, v);
    await vmAction(page, "Edit Settings", "Firmware and TPM");
    const dlg = dialog(page, "Firmware and TPM");
    await expect(dlg.getByText("Secure Boot")).toBeVisible();
    await expect(dlg.getByText("Hardware clock")).toHaveCount(0);
    await page.keyboard.press("Escape");
    expect((await clock(api, v)).windows).toBe(false);
    await expect(api.put(`/nodes/${v.node}/vms/${v.uuid}/clock`, { utc: true })).rejects.toThrow(/Windows guests/);
  });
});
