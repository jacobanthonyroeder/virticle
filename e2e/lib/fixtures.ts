import { expect, Page, test as base } from "@playwright/test";
import { Api, removeVM } from "./api";
import { env, P } from "./env";

type Worker = { api: Api; n1: number; n2: number };

export const test = base.extend<{ unused: void; noBrowserDialogs: void }, Worker>({
  // The app must never open a browser pop-up (alert, confirm, prompt): every question is an in-app dialog.
  noBrowserDialogs: [
    async ({ page }, use) => {
      const seen: string[] = [];
      page.on("dialog", (d) => {
        seen.push(`${d.type()}: ${d.message().slice(0, 80)}`);
        d.dismiss().catch(() => {});
      });
      await use();
      expect(seen, "browser pop-ups opened by the app").toEqual([]);
    },
    { auto: true },
  ],
  api: [
    async ({}, use) => {
      const api = await Api.login();
      await use(api);
      await api.dispose();
    },
    { scope: "worker" },
  ],
  n1: [async ({ api }, use) => use(await api.nodeId(env.node1)), { scope: "worker" }],
  n2: [async ({ api }, use) => use(await api.nodeId(env.node2)), { scope: "worker" }],
});
export { expect };

/** Creates a small VM from the cirros image through the API (the UI path is covered in vm-create.spec). */
export async function createCirros(api: Api, node: number, name: string, opts: { start?: boolean; memoryMiB?: number; network?: string } = {}) {
  const r = await api.post(`/nodes/${node}/vms`, {
    name,
    vcpus: 1,
    memoryMiB: opts.memoryMiB ?? 128,
    firmware: "bios",
    pool: "default",
    diskGiB: 1,
    baseVolume: env.cirros,
    network: opts.network ?? "default",
    start: opts.start ?? false,
  });
  return { node, uuid: r.uuid as string, name };
}

/** Deletes the named VMs (and their disks) if they exist. For afterAll hooks. */
export async function cleanVMs(api: Api, ...names: string[]) {
  const all = await api.vms();
  for (const n of names) {
    for (const vm of all.filter((v) => v.name === n)) await removeVM(api, vm);
  }
}

export const vmUrl = (node: number, uuid: string, tab?: string) => `/vm/${node}/${uuid}${tab ? "?tab=" + tab : ""}`;

/** Opens a VM detail page and waits for its header. */
export async function openVM(page: Page, vm: { node: number; uuid: string; name: string }, tab?: string) {
  await page.goto(vmUrl(vm.node, vm.uuid, tab));
  await expect(page.getByRole("heading", { level: 1, name: vm.name })).toBeVisible();
}

export async function tab(page: Page, name: string) {
  await page.locator(".tabs").getByRole("button", { name, exact: true }).click();
}

export async function stateOf(api: Api, vm: { node: number; uuid: string }) {
  return (await api.vmDetail(vm.node, vm.uuid)).state as string;
}

/**
 * Starts something that makes a task of the given kind (a call that may answer only when the task has ended),
 * stops that task through POST /api/tasks/{id}/cancel after `afterMs` and returns the finished task, the seconds
 * from the stop request to the end of the task, and what the call answered (or the error it ended with).
 */
export async function stopTask(api: Api, kind: string, start: () => Promise<unknown>, afterMs = 400) {
  type T = { id: number; kind: string; status: string; error?: string; cancel?: string };
  const tasks = async () => (await api.get("/tasks")) as T[];
  const known = new Set((await tasks()).map((t) => t.id));
  const call = start().then(
    (answer) => ({ answer, error: undefined as unknown }),
    (error) => ({ answer: undefined as unknown, error }),
  );
  const running = await api.waitFor(`a running ${kind} task`, async () => (await tasks()).find((t) => t.kind === kind && t.status === "running" && !known.has(t.id)), 60_000, 150);
  expect(running.cancel, `the running ${kind} task offers a stop`).toBeTruthy();
  await new Promise((r) => setTimeout(r, afterMs));
  const t0 = Date.now();
  await api.post(`/tasks/${running.id}/cancel`);
  const task = await api.waitFor(`the ${kind} task to end`, async () => (await tasks()).find((t) => t.id === running.id && t.status !== "running"), 30_000, 100);
  const seconds = (Date.now() - t0) / 1000;
  console.log(`  ${kind}: stopped ${afterMs} ms after it started, ended ${seconds.toFixed(1)} s after the request: ${task.status} "${task.error}"`);
  expect(task.status).toBe("failed");
  expect(task.error).toContain("cancelled");
  return { task, seconds, ...(await call) };
}

export { P };
