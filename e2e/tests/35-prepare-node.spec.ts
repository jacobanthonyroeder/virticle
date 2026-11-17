// The Add node dialog, Check only, against lab node 1. Read-only: the dialog signs in with an administrator
// key pasted into it, looks at the host and changes nothing (the Add node button is never pressed here). The key is a secret for this one run: after
// the form is submitted it must be nowhere on the page, in the task list or in the audit log.
//
// The administrator key is read from E2E_ADMIN_KEY_FILE (default: the lab's controller key, which is
// authorized for root on the lab nodes); the test is skipped when it cannot be read.
import { readFileSync } from "node:fs";
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";
import { dialog, field, pick } from "../lib/ui";

const KEY_FILE = process.env.E2E_ADMIN_KEY_FILE ?? "/root/virt-proj/lab/keys/controller";
let key = "";
try {
  key = readFileSync(KEY_FILE, "utf8").trim();
} catch {
  /* skipped below */
}

test.describe.serial("prepare node, check only", () => {
  test.skip(!key, `no administrator key at ${KEY_FILE}`);

  test("the dialog checks a node, shows the rows, and leaves no secret behind", async ({ page, api, n1 }) => {
    const node = (await api.nodes()).find((n) => n.id === n1) as any;
    const before = await api.get(`/nodes/${n1}`);
    const body = key.split("\n").filter((l) => !l.startsWith("-----"))[1] ?? key.slice(40, 80); // a line of the key itself
    expect(body.length).toBeGreaterThan(20);

    await page.goto("/nodes");
    await page.getByRole("button", { name: "Add node" }).click();
    // one dialog: address, administrator user and credential up front; the jump host is under Advanced
    const dlg = dialog(page, "Add node");
    await expect(dlg).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Prepare this node" })).toHaveCount(0);
    await expect(field(dlg, "Administrator user")).toHaveValue("root");
    await field(dlg, "Address").fill(node.addr);
    await dlg.getByText("Advanced", { exact: true }).click();
    await field(dlg, "Jump host (optional)").fill(node.jump ?? "");
    await pick(dlg, "Credential", "Private key");
    await dlg.locator("textarea").fill(key);
    // one checkbox only (Root access), and the buttons Cancel, Check and Add node
    await expect(dlg.getByRole("checkbox")).toHaveCount(1);
    await expect(dlg.getByLabel("Root access instead of a service account")).not.toBeChecked();
    await expect(dlg.getByRole("button", { name: "Add node", exact: true })).toBeVisible();
    // the direct form for a node prepared on the command line is one click away
    await dlg.getByRole("button", { name: "Already prepared: connect without an administrator login" }).click();
    await expect(field(dlg, "SSH user")).toHaveValue("virticle-node");
    await expect(dlg.locator("textarea, input[type=password]")).toHaveCount(0);
    await dlg.getByRole("button", { name: "Use an administrator login instead" }).click();
    await dlg.locator("textarea").fill(key);
    await dlg.getByRole("button", { name: "Check", exact: true }).click();

    // the form is gone and the secret with it
    const res = dialog(page, "Check node");
    await expect(res).toBeVisible();
    await expect(res.locator("textarea, input[type=password]")).toHaveCount(0);

    const list = res.locator("ul.checklist");
    await expect(list).toContainText("Administrator access", { timeout: 60_000 });
    for (const t of ["Architecture", "Distribution", "CPU virtualisation", "KVM device", "IOMMU", "Free space", "Security module", "Time sync", "libvirt and QEMU", "Other controller", "Existing VMs", "Service account"]) {
      await expect(list.locator("strong", { hasText: new RegExp(`^${t}$`) })).toBeVisible();
    }
    await expect(list).toContainText("Existing VMs are left alone");
    // the IOMMU row names the exact kernel arguments for this node and keeps where they go behind the help mark
    const iommu = list.locator("li", { hasText: "IOMMU" });
    await expect(iommu).toContainText(/(Off\. For device passthrough: add (intel|amd)_iommu=on iommu=pt to the kernel command line|On \(\d+ groups\))/);
    if (/Off/.test((await iommu.textContent()) ?? "")) {
      await iommu.getByRole("button", { name: "Help" }).hover();
      await expect(page.getByRole("tooltip")).toContainText("Virticle does not change the boot loader or reboot");
    }
    await expect(list).toContainText(/libvirt \d+\.\d+\.\d+ and QEMU \d+\.\d+\.\d+/); // the minimum is libvirt 11.0 and QEMU 10.0
    await expect(list).toContainText(/Debian GNU\/Linux 13|Rocky Linux 10/);
    await expect(res.getByRole("button", { name: "Add node", exact: true })).toBeVisible(); // Add node is there, without retyping the key
    await expect(list.locator("strong", { hasText: /^Service account$/ })).toBeVisible();
    await expect(res.getByRole("img", { name: "not run" })).toHaveCount(0);

    // no secret on the page, in the tasks or in the audit log
    await expect(page.locator("body")).not.toContainText(body);
    await expect(page.locator("body")).not.toContainText("PRIVATE KEY");
    const tasks = JSON.stringify(await api.get("/tasks"));
    expect(tasks).toContain("check node");
    expect(tasks).not.toContain(body);
    expect(tasks).not.toContain("PRIVATE KEY");
    const audit = JSON.stringify(await api.get("/audit"));
    expect(audit).toContain("prepare");
    expect(audit).not.toContain(body);
    expect(audit).not.toContain("PRIVATE KEY");
    const html = await page.content();
    expect(html).not.toContain(body);

    // nothing changed on the node or in the controller
    const after = await api.get(`/nodes/${n1}`);
    expect(after.privilege).toBe(before.privilege);
    expect((await api.nodes()).find((n) => n.id === n1)?.state).toBe("online");
    await page.keyboard.press("Escape");
  });

  // A jump host that does not accept the controller's key (a new controller): E2E_JUMP_NOKEY is user@address of an
  // account there that has no authorized_keys entry for it, E2E_JUMP_PASSWORD its password. The node (E2E_NODE1) must be
  // reachable from that jump host. Check only: nothing is written on the jump host.
  test("a jump host that refuses the controller's key asks for its own login", async ({ page, api, n1 }) => {
    test.skip(!process.env.E2E_JUMP_NOKEY || !process.env.E2E_JUMP_PASSWORD, "no jump account without the controller's key (E2E_JUMP_NOKEY, E2E_JUMP_PASSWORD)");
    const jumpPw = process.env.E2E_JUMP_PASSWORD as string;
    const node = (await api.nodes()).find((n) => n.id === n1) as any;
    await page.goto("/nodes");
    await page.getByRole("button", { name: "Add node" }).click();
    const dlg = dialog(page, "Add node");
    await field(dlg, "Address").fill(node.addr);
    await dlg.getByText("Advanced", { exact: true }).click();
    await field(dlg, "Jump host (optional)").fill(process.env.E2E_JUMP_NOKEY as string);
    // the jump host login is not asked for before the jump host refused the controller's key
    await expect(dlg.getByText("Jump host password")).toHaveCount(0);
    await pick(dlg, "Credential", "Private key");
    await dlg.locator("textarea").fill(key);
    await dlg.getByRole("button", { name: "Check", exact: true }).click();

    const res = dialog(page, "Check node");
    const row = res.locator("ul.checklist li", { hasText: "Jump host" });
    await expect(row).toContainText("the controller's key is not accepted", { timeout: 60_000 });
    await res.getByRole("button", { name: "Back" }).click();
    // the login is asked for now, with what it does behind the help mark
    const pwField = field(dlg, "Jump host password");
    await expect(pwField).toBeVisible();
    await dlg.locator("label.field", { hasText: "Jump host password" }).getByRole("button", { name: "About Jump host password" }).hover();
    await expect(page.getByRole("tooltip")).toContainText("restrict,port-forwarding");
    await pwField.fill("wrong-jump-password");
    await dlg.getByRole("button", { name: "Check", exact: true }).click();
    await expect(dialog(page, "Check node").locator("ul.checklist li", { hasText: "Jump host" })).toContainText("sign-in refused", { timeout: 60_000 });
    await dialog(page, "Check node").getByRole("button", { name: "Back" }).click();
    await pwField.fill(jumpPw);
    await dlg.getByRole("button", { name: "Check", exact: true }).click();

    const ok = dialog(page, "Check node");
    await expect(ok.locator("ul.checklist")).toContainText("Administrator access", { timeout: 60_000 });
    await expect(ok.locator("ul.checklist li", { hasText: "Jump host" })).toHaveCount(0); // a check adds nothing on the jump host
    await expect(ok.locator("ul.checklist li", { hasText: "Administrator access" }).getByRole("img", { name: "ok" })).toBeVisible();
    // the secrets are nowhere on the page, in the tasks or in the audit log
    for (const secret of [jumpPw, "wrong-jump-password"]) {
      await expect(page.locator("body")).not.toContainText(secret);
      expect(JSON.stringify(await api.get("/tasks"))).not.toContain(secret);
      expect(JSON.stringify(await api.get("/audit"))).not.toContain(secret);
    }
    await page.keyboard.press("Escape");
  });

  test("the API refuses a run without a credential and a bad account name", async ({ api }) => {
    const miss = await api.ctx.fetch("/api/prepare", { method: "POST", data: { addr: "192.0.2.1", checkOnly: true } });
    expect(miss.status()).toBe(400);
    expect((await miss.json()).error).toContain("password or private key");
    const bad = await api.ctx.fetch("/api/prepare", { method: "POST", data: { addr: "192.0.2.1", password: "x", user: "Bad Name", checkOnly: true } });
    expect(bad.status()).toBe(400);
    expect((await bad.json()).error).toContain("not a valid account name");
    const root = await api.ctx.fetch("/api/prepare", { method: "POST", data: { addr: "192.0.2.1", password: "x", user: "root", checkOnly: true } });
    expect(root.status()).toBe(400);
  });

  test("a wrong address ends as an error row and never as a half-done run", async ({ page, api }) => {
    const r = await api.ctx.fetch("/api/prepare?async=1", { method: "POST", data: { addr: "127.0.0.1:1", adminUser: "root", password: "secret-for-this-test-only", checkOnly: true } });
    expect(r.status()).toBe(202);
    const id = (await r.json()).task as number;
    await expect.poll(async () => (await api.get(`/prepare/${id}`)).done, { timeout: 60_000 }).toBe(true);
    const job = await api.get(`/prepare/${id}`);
    expect(job.ok).toBe(false);
    expect(job.rows[0].status).toBe("error");
    expect(JSON.stringify(job)).not.toContain("secret-for-this-test-only");
    expect(JSON.stringify(await api.get("/tasks"))).not.toContain("secret-for-this-test-only");
    expect(JSON.stringify(await api.get("/audit"))).not.toContain("secret-for-this-test-only");
    void page;
    void env;
  });
});
