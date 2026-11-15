// Migration network: the Settings tab and the Edit connection dialog offer the node's addresses as a list,
// the topology marks the address that carries migration, and the setting can be changed and reset.
// Read-only checks run on every lab. The change test needs a node with two addresses and skips otherwise;
// it always puts the setting back, also when it fails.
import { expect, test } from "../lib/fixtures";
import { Page } from "@playwright/test";
import { dialog, field, pick } from "../lib/ui";

/** Opens Edit on the switch or card that shows the address (the node's Network tab must be open). */
async function openEdit(page: Page, ip: string) {
  const head = page.locator("section.toposwitch > header").filter({ hasText: `${ip}/` });
  const card = page.locator(".topouplink").filter({ hasText: `${ip}/` });
  await expect(head.or(card).first()).toBeVisible({ timeout: 30_000 });
  const btn = (await head.count()) > 0 ? head.locator("[data-topo-edit]") : card.locator("[data-topo-edit]");
  await btn.first().click();
  const dlg = page.getByRole("dialog");
  await expect(dlg.getByRole("checkbox", { name: `Migration on ${ip}` })).toBeVisible({ timeout: 30_000 });
  return dlg;
}

interface Option {
  interface: string;
  address: string;
  label: string;
}
interface Info {
  address: string;
  limit: string;
  show: boolean;
  options: Option[];
  management: { address: string; label: string };
  current: string;
  limitText: string;
}

test.describe.serial("migration network", () => {
  test("the choices name the interface, the address and the speed when known", async ({ api, n1 }) => {
    const info: Info = await api.get(`/nodes/${n1}/migration-network`);
    expect(info.management.label).toBe(`Same as management (${info.management.address})`);
    for (const o of info.options) {
      expect(o.label).toContain(`${o.interface} (${o.address})`);
      expect(o.label).toMatch(/^[\w.:-]+ \([0-9a-f.:]+\)(, [\d.]+ [GM]b\/s)?$/);
    }
    expect(info.show).toBe(info.options.length > 0 || info.address !== "");
    expect(info.limitText).toMatch(/^(80% of the link \(default\)|No limit|Custom \(\d+ Mbit\/s\))$/);
  });

  test("the Settings tab shows the row only when there is something to choose", async ({ page, api, n1 }) => {
    const info: Info = await api.get(`/nodes/${n1}/migration-network`);
    await page.goto(`/node/${n1}/settings`);
    const card = page.locator(".card", { has: page.getByRole("heading", { name: "Connection", exact: true }) });
    await expect(card).toBeVisible();
    const row = card.locator("dt", { hasText: /^Migration network$/ });
    if (info.show) {
      await expect(row).toBeVisible();
      await expect(card).toContainText(info.current);
      await expect(card.getByRole("button", { name: "Change the migration network" })).toBeVisible();
    } else {
      await expect(row).toHaveCount(0);
    }
    await card.getByText("Details", { exact: true }).click();
    await expect(card.locator("dt", { hasText: /^Limit on a shared link$/ })).toBeVisible();
    await expect(card).toContainText(info.limitText);
  });

  test("Edit connection has the migration network and the limit under Advanced", async ({ page, api, n1 }) => {
    const info: Info = await api.get(`/nodes/${n1}/migration-network`);
    await page.goto(`/node/${n1}/settings`);
    await page.getByRole("button", { name: "Edit connection" }).click();
    const dlg = dialog(page, "Edit connection");
    await expect(dlg.getByText("Advanced", { exact: true })).toHaveCount(1);
    await expect(field(dlg, "Migration address (optional)")).toHaveCount(0); // replaced by the list
    await dlg.getByText("Advanced", { exact: true }).click();
    if (info.show) await expect(field(dlg, "Migration network")).toBeVisible();
    else await expect(field(dlg, "Migration network")).toHaveCount(0);
    await field(dlg, "Limit on a shared link").click();
    const list = page.getByRole("listbox").last();
    for (const t of ["80% of the link (default)", "No limit", "Custom (Mbit/s)"]) await expect(list.getByRole("option", { name: t })).toBeVisible();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
  });

  test("the pre-flight says which network the data uses and whether it is limited", async ({ api, n1, n2 }) => {
    const vms = (await api.vms()).filter((v) => v.node === n1 && v.state === "shutoff");
    test.skip(vms.length === 0, "needs a stopped VM on the first node to run the pre-flight against");
    const r = await api.post(`/nodes/${n1}/vms/${vms[0].uuid}/migrate/check`, { target: n2, live: false });
    const rows: { check: string; message: string }[] = r.checks;
    const net = rows.find((c) => c.check === "Migration network");
    expect(net, "a Migration network row").toBeTruthy();
    expect(net!.message).toMatch(/ on both nodes \(|management network on/);
    const lim = rows.find((c) => c.check === "Transfer limit");
    if (lim) expect(lim.message).toMatch(/^(Shared with management( on \S+)?: limited to \d+ Mbit\/s \(.+\)\.|Link speed unknown, so the transfer is not limited\.|Shared with management, but the limit is turned off.*)$/);
  });

  test("Edit on the topology shows management and migration as checkboxes", async ({ page, api, n1 }) => {
    const info: Info = await api.get(`/nodes/${n1}/migration-network`);
    test.skip(info.address !== "", "the node already has a migration network set");
    await page.goto(`/node/${n1}/network`);
    await expect(page.locator(".topomgmt", { hasText: /^\s*Migration\s*$/ })).toHaveCount(0); // no mark while it is the default
    const ip = info.management.address;
    const dlg = await openEdit(page, ip);
    // management: ticked and fixed here; migration follows it by default
    await expect(dlg.getByRole("checkbox", { name: `Management on ${ip}` })).toBeChecked();
    await expect(dlg.getByRole("checkbox", { name: `Management on ${ip}` })).toBeDisabled();
    await expect(dlg.getByRole("checkbox", { name: `Migration on ${ip}` })).toBeChecked();
    await expect(dlg.getByRole("button", { name: "Save" })).toBeDisabled(); // nothing changed yet
    await dlg.getByRole("button", { name: "Cancel" }).click();
    if (info.options.length > 0) {
      const other = await openEdit(page, info.options[0].address);
      await expect(other.getByRole("checkbox", { name: `Migration on ${info.options[0].address}` })).not.toBeChecked();
      await other.getByRole("button", { name: "Cancel" }).click();
    }
    await expect(page.getByText("Use for migration")).toHaveCount(0);
  });

  test("a libvirt NAT bridge offers no Management box", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/network`);
    const sw = page.locator("section.toposwitch").filter({ hasText: "libvirt network bridge" });
    await expect(page.locator("section.toposwitch").first()).toBeVisible({ timeout: 30_000 });
    test.skip((await sw.count()) === 0 || (await sw.first().locator("[data-topo-edit]").count()) === 0, "the node has no libvirt bridge with an address");
    await sw.first().locator("[data-topo-edit]").click();
    const dlg = page.getByRole("dialog");
    await expect(dlg.getByRole("checkbox", { name: /^Migration on / }).first()).toBeVisible({ timeout: 30_000 });
    await expect(dlg.getByRole("checkbox", { name: /^Management on / })).toHaveCount(0);
    await dlg.getByRole("button", { name: "Cancel" }).click();
  });

  test("setting the migration network and resetting it", async ({ page, api, n1 }) => {
    const before: Info = await api.get(`/nodes/${n1}/migration-network`);
    test.skip(before.options.length === 0, "needs a node with two addresses");
    const target = before.options[0];
    try {
      await page.goto(`/node/${n1}/settings`);
      await page.getByRole("button", { name: "Change the migration network" }).click();
      const dlg = dialog(page, "Migration network");
      await pick(dlg, "Migration network", target.label);
      await dlg.getByRole("button", { name: "Save" }).click();
      await expect(dlg).toBeHidden({ timeout: 90_000 });
      await expect(page.getByRole("list", { name: "Result of the change" })).toContainText("Migration settings saved.");
      const after: Info = await api.get(`/nodes/${n1}/migration-network`);
      expect(after.address).toBe(target.address);
      expect(after.current).toBe(target.label);
      await expect(page.locator(".card", { has: page.getByRole("heading", { name: "Connection", exact: true }) })).toContainText(target.label);
      // the topology now marks it
      await page.goto(`/node/${n1}/network`);
      await expect(page.locator(".topomgmt", { hasText: /^\s*Migration\s*$/ }).first()).toBeVisible({ timeout: 30_000 });
      // the encryption certificate covers the new address when encrypted migration is set up
      const tls = await api.get(`/nodes/${n1}/migration-tls`);
      if (tls.state !== "not-set-up") expect(["ok", "valid"]).toContain(tls.state);
      // reset where it was set: untick Migration in Edit
      let ed = await openEdit(page, target.address);
      await expect(ed.getByRole("checkbox", { name: `Migration on ${target.address}` })).toBeChecked();
      await ed.getByRole("checkbox", { name: `Migration on ${target.address}` }).uncheck();
      await ed.getByRole("button", { name: "Save" }).click();
      await expect(page.locator(".migresult")).toContainText("Migration network saved.", { timeout: 90_000 });
      expect((await api.get(`/nodes/${n1}/migration-network`)).address).toBe("");
      // and set it again by ticking it, then back by ticking Migration on the management address
      await page.reload();
      ed = await openEdit(page, target.address);
      await ed.getByRole("checkbox", { name: `Migration on ${target.address}` }).check();
      await ed.getByRole("button", { name: "Save" }).click();
      await expect(page.locator(".migresult")).toContainText("Migration network saved.", { timeout: 90_000 });
      expect((await api.get(`/nodes/${n1}/migration-network`)).address).toBe(target.address);
      await page.reload();
      ed = await openEdit(page, before.management.address);
      await ed.getByRole("checkbox", { name: `Migration on ${before.management.address}` }).check();
      await ed.getByRole("button", { name: "Save" }).click();
      await expect(page.locator(".migresult")).toContainText("Migration network saved.", { timeout: 90_000 });
      expect((await api.get(`/nodes/${n1}/migration-network`)).address).toBe("");
    } finally {
      await api.put(`/nodes/${n1}/migration-network`, { address: before.address, limit: before.limit });
    }
    const end: Info = await api.get(`/nodes/${n1}/migration-network`);
    expect(end.address).toBe(before.address);
    expect(end.limit).toBe(before.limit);
  });

  test("the limit on a shared link can be set and put back", async ({ api, n1 }) => {
    const before: Info = await api.get(`/nodes/${n1}/migration-network`);
    try {
      let res = await api.put(`/nodes/${n1}/migration-network`, { limit: "custom:500" });
      expect(res.limitText).toBe("Custom (500 Mbit/s)");
      res = await api.put(`/nodes/${n1}/migration-network`, { limit: "none" });
      expect(res.limitText).toBe("No limit");
      const bad = await api.ctx.fetch(`/api/nodes/${n1}/migration-network`, { method: "PUT", data: { limit: "custom:0" } });
      expect(bad.status()).toBe(400);
    } finally {
      await api.put(`/nodes/${n1}/migration-network`, { limit: before.limit });
    }
    expect((await api.get(`/nodes/${n1}/migration-network`)).limit).toBe(before.limit);
  });
});
