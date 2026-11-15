// Verify on a backup row: a plain chain of three is intact, an encrypted backup is checked without a passphrase, and
// on the lab (SSHPASS set) a flipped byte in an earlier backup and a missing earlier backup are named. The row fits
// on a phone. The damage is made in this spec's own backups on node 1 and undone before they are deleted.
import { execFileSync } from "child_process";
import { resolve } from "path";
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog } from "../lib/ui";
import { Api } from "../lib/api";
import { labAvailable } from "../lib/lab";

const NAME = `${P}ver1`;
const PASS = process.env.E2E_BACKUP_PASS ?? "e2e backup key passphrase";
const root = process.env.E2E_LAB_ROOT ?? resolve(__dirname, "../..");

/** Runs a shell command on lab node 1 (ssh alias E2E_SSH_NODE1). */
const nodeSh = (cmd: string) => execFileSync("ssh", ["-F", "lab/ssh_config", "-o", "ConnectTimeout=20", env.sshNode1, cmd], { cwd: root, encoding: "utf8", timeout: 60_000 });

async function backups(api: Api, uuid: string) {
  return ((await api.get(`/backups?uuid=${uuid}`)) as any[]).sort((a, b) => a.manifest.created.localeCompare(b.manifest.created));
}

async function backUp(api: Api, vm: { node: number; uuid: string }) {
  const r = await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/backup`, { pool: "default", keepLast: 0 });
  await api.waitFor("the backup", async () => ((await api.get("/tasks")) as any[]).find((t) => t.id === r.task && t.status !== "running"), 200_000, 1000);
}

/** Opens Verify on the row of backup dir and waits for the result word. */
async function verify(page: Page, dir: string, word: "Intact" | "Damaged") {
  const row = page.locator("tbody tr").filter({ has: page.locator(`td[title$="${dir}"]`) });
  await row.getByRole("button", { name: "Verify", exact: true }).click();
  const dlg = dialog(page, `Verify ${NAME}`);
  await expect(dlg.locator(".badge")).toHaveText(word, { timeout: 120_000 });
  return dlg;
}

test.describe.serial("verify backup", () => {
  let vm: { node: number; uuid: string; name: string };
  let dirs: string[] = [];

  test.beforeAll(async ({ api, n1 }) => {
    test.setTimeout(600_000);
    await api.post("/backup-encryption/off").catch(() => {});
    vm = await createCirros(api, n1, NAME, { start: true });
    for (let i = 0; i < 3; i++) await backUp(api, vm);
    dirs = (await backups(api, vm.uuid)).map((b) => b.dir);
    expect(dirs).toHaveLength(3);
  });

  test.afterAll(async ({ api }) => {
    await api.post("/backup-encryption/off").catch(() => {});
    await cleanVMs(api, NAME);
    if (vm) {
      for (const b of (await backups(api, vm.uuid)).reverse()) {
        await api.del(`/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}?withLater=1`).catch(() => {});
      }
    }
  });

  test("a chain of three is intact", async ({ page }) => {
    await openVM(page, vm, "backups");
    const dlg = await verify(page, dirs[2], "Intact");
    await expect(dlg).toContainText(/\d+ files intact in 3 backups/);
    await expect(dlg.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "100");
    await dlg.getByRole("button", { name: "Close dialog" }).click();
    const t = ((await page.request.get("/api/tasks").then((r) => r.json())) as any[]).find((x) => x.kind === "verify" && x.target === dirs[2]);
    expect(t).toMatchObject({ status: "ok", node: env.node1 });
  });

  test("a flipped byte and a missing earlier backup are named", async ({ page }) => {
    test.skip(!labAvailable(), "needs the lab shell (SSHPASS)");
    const list = await page.request.get(`/api/backups?uuid=${vm.uuid}`).then((r) => r.json());
    const pool = (list as any[])[0].path.replace(/\/[^/]+$/, "");
    const mid = `${pool}/${dirs[1]}`;
    // invert the byte at 100000 of the middle backup's disk image; the size stays
    const flip = `f=${mid}/vda.qcow2; b=$(dd if=$f bs=1 skip=100000 count=1 2>/dev/null | od -An -tu1 | tr -d ' '); printf "\\\\$(printf %o $((b ^ 255)))" | dd of=$f bs=1 seek=100000 count=1 conv=notrunc 2>/dev/null`;
    nodeSh(flip);
    try {
      await openVM(page, vm, "backups");
      let dlg = await verify(page, dirs[2], "Damaged");
      await expect(dlg.locator(".error")).toContainText(`${dirs[1]}/vda.qcow2 is damaged: its SHA-256 differs`);
      await dlg.getByRole("button", { name: "Close dialog" }).click();
      // the full backup underneath is still intact
      dlg = await verify(page, dirs[0], "Intact");
      await dlg.getByRole("button", { name: "Close dialog" }).click();
    } finally {
      nodeSh(flip);
    }
    nodeSh(`mv ${mid} ${pool}/${P}ver1-hidden`);
    try {
      await openVM(page, vm, "backups");
      const dlg = await verify(page, dirs[2], "Damaged");
      await expect(dlg.locator(".error")).toContainText(`the earlier backup ${dirs[1]} that this one builds on is missing`);
      await dlg.getByRole("button", { name: "Close dialog" }).click();
    } finally {
      nodeSh(`mv ${pool}/${P}ver1-hidden ${mid}`);
    }
    await page.reload();
    const dlg = await verify(page, dirs[2], "Intact");
    await dlg.getByRole("button", { name: "Close dialog" }).click();
  });

  test("an encrypted backup is verified without the passphrase", async ({ page, api }) => {
    test.setTimeout(300_000);
    await api.post("/backup-encryption/on", { passphrase: PASS, again: PASS });
    await backUp(api, vm);
    await api.post("/backup-encryption/off");
    const newest = (await backups(api, vm.uuid)).pop();
    expect(newest.manifest.recipient).toBeTruthy();
    await openVM(page, vm, "backups");
    const dlg = await verify(page, newest.dir, "Intact");
    await expect(dlg).toContainText("files intact in 4 backups");
    await expect(dlg.locator("input[type=password]")).toHaveCount(0);
    await dlg.getByRole("button", { name: "Close dialog" }).click();
  });

  test("phone width: the backup rows fit", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openVM(page, vm, "backups");
    await expect(page.getByRole("button", { name: "Verify", exact: true }).first()).toBeVisible();
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(over).toBeLessThanOrEqual(0);
  });
});
