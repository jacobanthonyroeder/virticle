// Settings > Backup encryption: turn it on with a passphrase, the dots turn green, a VM backup is encrypted and says so,
// its restore asks for the passphrase (a wrong one is refused, the right one restores a VM that boots), turn it off:
// the next backup is not encrypted and the encrypted one still restores. The key stays when encryption is turned
// off, so a later run finds it and turns encryption on with the same passphrase (E2E_BACKUP_PASS). The spec leaves
// encryption off.
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, field } from "../lib/ui";
import { Api } from "../lib/api";

const NAME = `${P}enc1`;
const R1 = `${P}enc1-r1`;
const R2 = `${P}enc1-r2`;
const PASS = process.env.E2E_BACKUP_PASS ?? "e2e backup key passphrase";

const card = (page: Page) => page.getByRole("region", { name: "Encryption" });
const headDot = (page: Page) => page.locator(".headtitle .badge");

async function noSideways(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

async function backupNow(page: Page) {
  await page.getByRole("button", { name: "Back up now" }).first().click();
  const dlg = dialog(page, `Back up ${NAME}`);
  await expect(dlg.getByLabel("Target pool")).toContainText("default");
  await dlg.getByRole("button", { name: "Back up now" }).click();
  await expect(dlg).toContainText(/ok/i, { timeout: 200_000 });
  await dlg.getByRole("button", { name: "Close dialog" }).click();
}

async function backups(api: Api, uuid: string) {
  return ((await api.get(`/backups?uuid=${uuid}`)) as any[]).sort((a, b) => a.manifest.created.localeCompare(b.manifest.created));
}

test.describe.serial("backup encryption", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => {
    await api.post("/backup-encryption/off").catch(() => {});
    await cleanVMs(api, R1, R2, NAME);
    if (vm) {
      for (const b of (await backups(api, vm.uuid)).reverse()) {
        await api.del(`/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}?withLater=1`).catch(() => {});
      }
    }
  });

  test("turning encryption on asks for a passphrase and turns the dots green", async ({ page, api }) => {
    await api.post("/backup-encryption/off");
    await page.goto("/");
    await page.locator(".topbar").getByRole("button", { name: "Settings" }).click();
    await page.getByRole("menuitem", { name: "Backup encryption", exact: true }).click();
    await expect(page).toHaveURL(/\/backup-encryption$/);
    await expect(headDot(page)).toHaveText("Not encrypted");
    await expect(page.getByText("encrypted backups cannot be restored by anyone")).toBeVisible();
    const st = await api.get("/backup-encryption");
    const c = card(page);
    if (st.hasKey) {
      // A key from an earlier run: it is kept, and turning on needs its passphrase.
      await field(c, "Passphrase of the backup key").fill("not the passphrase");
      await c.getByRole("button", { name: "Turn on encryption" }).click();
      await expect(c.getByText("wrong passphrase for the backup key")).toBeVisible();
      await field(c, "Passphrase of the backup key").fill(PASS);
      await c.getByRole("button", { name: "Turn on encryption" }).click();
    } else {
      await field(c, "Passphrase").fill(PASS);
      await field(c, "Passphrase again").fill("something else entirely");
      await c.getByRole("button", { name: "Turn on encryption" }).click();
      await expect(c.getByText("The two passphrases differ.")).toBeVisible();
      await field(c, "Passphrase again").fill(PASS);
      const dl = page.waitForEvent("download");
      await c.getByRole("button", { name: "Turn on encryption" }).click();
      // The recovery key file comes at once.
      expect((await dl).suggestedFilename()).toBe("virticle-backup-key.age");
    }
    await expect(headDot(page)).toHaveText("Encrypted");
    await expect(headDot(page)).toHaveClass(/s-encrypted/);
    await expect(c.getByRole("button", { name: "Turn off encryption" })).toBeVisible();
    await expect(page.getByRole("region", { name: "Recovery key file" }).locator("pre")).toContainText("age -d -i virticle-backup-key.age");
    // The same dot on the backup pages.
    await page.goto("/datacenter/backups");
    await expect(headDot(page)).toHaveText("Encrypted");
    await page.goto("/controller-backup");
    await expect(headDot(page)).toHaveText("Encrypted");
  });

  test("a backup made now is encrypted and the list says so", async ({ page, api, n1 }) => {
    test.setTimeout(300_000);
    vm = await createCirros(api, n1, NAME, { start: true });
    await openVM(page, vm, "backups");
    await expect(headDot(page)).toHaveText("Encrypted");
    await backupNow(page);
    const [b] = await backups(api, vm.uuid);
    expect(b.manifest.recipient).toMatch(/^age1/);
    expect(b.manifest.disks[0].file).toMatch(/\.qcow2\.age$/);
    expect(b.manifest.disks[0].encrypted).toBe(true);
    expect(b.needsKey).toBe(true);
    await expect(page.locator("tbody tr").first().locator(".badge.s-encrypted")).toHaveText("Encrypted");
  });

  test("the restore asks for the passphrase: a wrong one is refused, the right one restores a VM that boots", async ({ page, api }) => {
    test.setTimeout(300_000);
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Restore" }).click();
    const dlg = dialog(page, `Restore ${NAME}`);
    await dlg.getByLabel("New VM name").fill(R1);
    const restore = dlg.getByRole("button", { name: "Restore", exact: true });
    await expect(restore).toBeDisabled(); // no passphrase yet
    await field(dlg, "Backup key passphrase").fill("not the passphrase");
    await restore.click();
    await expect(dlg.getByText("wrong passphrase for the backup key")).toBeVisible();
    expect(await api.vmByName(R1)).toBeUndefined();
    await field(dlg, "Backup key passphrase").fill(PASS);
    await restore.click();
    await expect(dlg).toContainText(/ok/i, { timeout: 250_000 });
    const r = await api.waitFor("restored VM", () => api.vmByName(R1), 30_000);
    await api.waitFor("restored VM running", async () => (await api.vmDetail(r.node, r.uuid)).state === "running", 60_000);
  });

  test("turned off: the dots go grey, the next backup is not encrypted, the encrypted one still restores", async ({ page, api }) => {
    test.setTimeout(400_000);
    await page.goto("/backup-encryption");
    await card(page).getByRole("button", { name: "Turn off encryption" }).click();
    await dialog(page, "Turn off encryption").getByRole("button", { name: "Turn off" }).click();
    await expect(headDot(page)).toHaveText("Not encrypted");
    await expect(headDot(page)).toHaveClass(/s-unencrypted/);
    await expect(card(page).getByRole("status")).toContainText(/\d+ encrypted backups? still needs? the key/);
    await openVM(page, vm, "backups");
    await expect(headDot(page)).toHaveText("Not encrypted");
    await backupNow(page);
    const list = await backups(api, vm.uuid);
    expect(list).toHaveLength(2);
    expect(list[1].manifest.recipient).toBeUndefined();
    expect(list[1].manifest.disks[0].file).toMatch(/\.qcow2$/);
    const rows = page.locator("tbody tr");
    await expect(rows.locator(".badge.s-unencrypted")).toHaveCount(1);
    await expect(rows.locator(".badge.s-encrypted")).toHaveCount(1);
    // the encrypted one still restores, with the passphrase
    await rows.filter({ has: page.locator(".badge.s-encrypted") }).getByRole("button", { name: "Restore" }).click();
    const dlg = dialog(page, `Restore ${NAME}`);
    await dlg.getByLabel("New VM name").fill(R2);
    await field(dlg, "Backup key passphrase").fill(PASS);
    await dlg.getByRole("button", { name: "Restore", exact: true }).click();
    await expect(dlg).toContainText(/ok/i, { timeout: 250_000 });
    await api.waitFor("second restored VM", () => api.vmByName(R2), 30_000);
  });

  test("the encryption page and the backup list fit a phone", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 800 });
    await page.goto("/backup-encryption");
    await expect(card(page)).toBeVisible();
    await noSideways(page);
    await openVM(page, vm, "backups");
    await expect(headDot(page)).toBeVisible();
    await noSideways(page);
  });
});
