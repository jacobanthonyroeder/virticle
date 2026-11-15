// What a request and a task ran on the node: the Audit row and the Tasks row open to their operations.
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { pick } from "../lib/ui";

const VM = P + "ops";

async function noSideScroll(page: Page) {
  const over = await page.evaluate(() => {
    const m = document.querySelector("main");
    return { doc: document.documentElement.scrollWidth - document.documentElement.clientWidth, main: m ? m.scrollWidth - m.clientWidth : 0 };
  });
  expect(over.doc, "page scrolls sideways").toBeLessThanOrEqual(0);
  expect(over.main, "main area scrolls sideways").toBeLessThanOrEqual(0);
}

test.describe("operations", () => {
  test.describe.configure({ mode: "serial" });
  let vm: { node: number; uuid: string; name: string };

  test.beforeAll(async ({ api, n1 }) => {
    await cleanVMs(api, VM);
    vm = await createCirros(api, n1, VM);
    await api.post(`/nodes/${n1}/vms/${vm.uuid}/action`, { action: "start" });
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
  });

  test("an audit row opens to the libvirt calls with their virsh equivalents", async ({ page, n1 }) => {
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto("/audit");
      await page.getByLabel("Filter by path").fill(`/api/nodes/${n1}/vms`);
      await pick(page, "Filter by method", "POST");
      await page.getByRole("button", { name: "Search" }).click();
      const row = page.locator("tbody tr").filter({ hasText: new RegExp(`POST /api/nodes/${n1}/vms(?!/)`) }).first();
      const open = row.getByRole("button", { name: /operations?$/ });
      await expect(open).toHaveAttribute("aria-expanded", "false");
      await open.click();
      await expect(open).toHaveAttribute("aria-expanded", "true");
      const list = page.locator(".oplist").first();
      const define = list.locator(".op").filter({ hasText: "virsh define domain.xml" });
      await expect(define).toContainText("virDomainDefineXMLFlags");
      await expect(define).toContainText("equivalent");
      await expect(define).toContainText("OK");
      await expect(define.getByRole("button", { name: "Copy virsh define domain.xml --validate" })).toBeVisible();
      await define.getByRole("button", { name: "domain.xml", exact: true }).click();
      await expect(define.locator(".op-xml")).toContainText(`<name>${VM}</name>`);
      await expect(list.locator(".op").filter({ hasText: "virsh vol-create-from default volume.xml" })).toContainText("virStorageVolCreateXMLFrom");
      await expect(list).not.toContainText("...");
      await noSideScroll(page);
    }
  });

  test("a task opens to its operations on the Tasks page", async ({ page }) => {
    await page.goto("/datacenter/tasks");
    const row = page.locator("tbody tr").filter({ hasText: "start" }).filter({ hasText: VM }).first();
    await row.getByRole("button", { name: "1 operation" }).click();
    await expect(page.locator(".oplist").first().locator(".op")).toContainText(`virsh start ${VM}`);
    await noSideScroll(page);
  });
});

