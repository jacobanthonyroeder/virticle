// Nodes page after an upgrade: a node prepared by an older version shows its state (dot and word) in the list, one
// notice above the list counts them and "Prepare all" prepares them one after another with one login; a node that
// fails does not stop the others. The node list and the preparation are answered by the test (nothing on the
// nodes is changed), so the spec runs on any lab.
import { Page, Route } from "@playwright/test";
import { expect, test } from "../lib/fixtures";
import { dialog } from "../lib/ui";

const NOTE = 'root: the installed node command is version 1 and this controller ships 2: run "virticle node prepare" to update it';

async function noSideways(page: Page) {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(over).toBeLessThanOrEqual(0);
}

test.describe.serial("prepare all", () => {
  let old = true;
  const started: any[] = [];

  test.beforeEach(async ({ page }) => {
    await page.route("**/api/nodes", async (route: Route) => {
      if (route.request().method() !== "GET") return route.continue();
      const res = await route.fetch();
      const body = await res.json();
      for (const n of body.nodes) if (n.state === "online") Object.assign(n, old ? { detectedPrivilege: "none", privilegeNote: NOTE } : { detectedPrivilege: "root", privilegeNote: "" });
      await route.fulfill({ response: res, json: body });
    });
    await page.route("**/api/prepare?async=1", async (route: Route) => {
      started.push(route.request().postDataJSON());
      await route.fulfill({ json: { task: 9000 + started.length } });
    });
    await page.route("**/api/prepare/900*", async (route: Route) => {
      const id = Number(route.request().url().split("/").pop());
      const first = id === 9001;
      const user = started[id - 9001]?.allowRoot ? "root" : "virticle-node";
      await route.fulfill({
        json: {
          rows: first ? [{ id: "admin", title: "Administrator access", status: "error", detail: "could not sign in: permission denied" }] : [{ id: "node", title: "Node command", status: "ok" }],
          done: true,
          ok: !first,
          checkOnly: false,
          user,
          addr: "x",
        },
      });
    });
  });

  test("the list shows the state and one notice with the count", async ({ page }) => {
    await page.goto("/nodes");
    await expect(page.locator(".prepbanner")).toBeVisible();
    const online = await page.locator("tbody tr").count();
    expect(online).toBeGreaterThanOrEqual(2);
    await expect(page.locator(".prepbanner")).toContainText(`${online} nodes need preparing.`);
    await expect(page.locator("tbody .badge.s-needs-prepare")).toHaveCount(online);
    await expect(page.locator("tbody .badge.s-needs-prepare").first()).toHaveText("Prepared by older version");
    await expect(page.locator(".prepbanner").getByRole("button", { name: "Prepare all" })).toBeVisible();
    await noSideways(page);
  });

  test("the datacenter Nodes tab shows it as well", async ({ page }) => {
    await page.goto("/datacenter/nodes");
    await expect(page.locator(".prepbanner")).toContainText("need preparing");
  });

  test("Prepare all runs every node, a failure does not stop the rest, and the notice goes", async ({ page }) => {
    started.length = 0;
    await page.goto("/nodes");
    await page.getByRole("button", { name: "Prepare all" }).click();
    const dlg = dialog(page, "Prepare all");
    await dlg.getByLabel("Password").fill("secret");
    await dlg.getByRole("button", { name: "Prepare all" }).click();
    await expect(dlg.locator(".badge.s-failed")).toHaveText("Failed");
    await expect(dlg).toContainText("could not sign in: permission denied");
    await expect(dlg.locator(".badge.s-ok").first()).toHaveText("Prepared");
    await noSideways(page);
    expect(started.length).toBeGreaterThanOrEqual(2);
    expect(started[0].adminUser).toBe("root");
    expect(started[0].password).toBe("secret");
    expect(new Set(started.map((s) => s.addr)).size).toBe(started.length);
    old = false;
    await dlg.getByRole("button", { name: "Close dialog" }).click();
    await page.reload();
    await expect(page.locator("tbody .badge.s-online").first()).toBeVisible();
    await expect(page.locator(".prepbanner")).toHaveCount(0);
    await expect(page.locator(".badge.s-needs-prepare")).toHaveCount(0);
  });
});
