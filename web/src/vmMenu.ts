// The one definition of a VM's action menu. The VM page's Actions button and the tree's right-click menu
// both call vmMenu(); they differ only in the handlers they pass. Entries that do not apply to the VM's
// state are disabled with a reason, never hidden, so the menu keeps its shape.
//
// Every entry has an id. A handler runs the action where the caller is (a dialog on the VM page, the call
// itself in the tree) or, in the tree, goes to the VM's page with "?do=<id>", where findEntry() runs the
// same entry. A new action needs a handler in VMMenuHandlers: both callers then fail to compile until
// they have one.
import type { MenuEntry } from "./Menu";
import { powerLabel, PowerAction } from "./power";
import type { HwArea } from "./pages/Hardware";

/** Hardware areas of Edit Settings: menu label and the section its dialog shows. */
export const hardwareAreas: [string, HwArea][] = [
  ["CPU and memory", "cpu"],
  ["Disks", "disks"],
  ["Network interfaces", "nics"],
  ["CD-ROM", "cdrom"],
  ["Boot order", "boot"],
  ["Firmware and TPM", "firmware"],
  ["Passthrough devices", "passthrough"],
];

/** What the menu needs to know about the VM. `agent` and `disks` come from the VM's detail; the tree's list does not carry them. */
export interface MenuVM {
  name: string;
  state: string;
  template: boolean;
  agent?: "connected" | "disconnected" | "absent";
  disks?: { target: string; device: string; source?: string }[];
}

/** The arguments of each action's handler (after the entry's id). */
interface Actions {
  power: [PowerAction];
  clock: [];
  edit: [label: string, area: HwArea];
  takeSnapshot: [];
  snapshots: [];
  schedule: [];
  clone: [];
  convert: [];
  convertBack: [];
  migrate: [];
  moveDisk: [target: string];
  replication: [];
  xml: [];
  remove: [];
}
export type VMMenuHandlers = { [K in keyof Actions]: (id: string, ...a: Actions[K]) => void };

export interface VMMenuCtx {
  vm: MenuVM;
  /** The user has at least the operator role. */
  op: boolean;
  /** Another call on this VM is running. */
  busy: boolean;
}

/** The first entry (at any depth) with this id, or undefined. */
export function findEntry(items: MenuEntry[], id: string): Exclude<MenuEntry, "sep"> | undefined {
  for (const it of items) {
    if (it === "sep") continue;
    if (it.id === id) return it;
    const sub = it.items && findEntry(it.items, id);
    if (sub) return sub;
  }
  return undefined;
}

export function vmMenu({ vm, op, busy }: VMMenuCtx, h: VMMenuHandlers): MenuEntry[] {
  const off = vm.state === "shutoff";
  const running = vm.state === "running";
  const paused = vm.state === "paused";
  const need = op ? undefined : "Needs the operator role";
  const when = (ok: boolean, why: string) => ({ disabled: !op || busy || !ok, title: need ?? (ok ? undefined : why) });
  const stateNow = off ? "The VM is shut off" : `The VM is ${vm.state}`;
  const loading = "Checking the guest agent";
  const agent = vm.agent === "connected";
  const agentWhy = vm.agent === undefined ? loading : "Needs the guest agent to be connected";

  const power: MenuEntry[] = [
    { id: "power:start", label: powerLabel.start, run: () => h.power("power:start", "start"), ...when(off && !vm.template, vm.template ? "A template cannot be started" : "The VM is already on") },
    { id: "power:reboot", label: powerLabel.reboot, run: () => h.power("power:reboot", "reboot"), ...when(running, stateNow) },
    { id: "power:shutdown", label: powerLabel.shutdown, run: () => h.power("power:shutdown", "shutdown"), ...when(running, stateNow) },
    "sep",
    { id: "power:pause", label: powerLabel.pause, run: () => h.power("power:pause", "pause"), ...when(running, stateNow) },
    { id: "power:resume", label: powerLabel.resume, run: () => h.power("power:resume", "resume"), ...when(paused, "The VM is not paused") },
    "sep",
    { id: "power:poweroff", label: powerLabel.poweroff, danger: true, run: () => h.power("power:poweroff", "poweroff"), ...when(!off, stateNow) },
    { id: "power:reset", label: powerLabel.reset, danger: true, run: () => h.power("power:reset", "reset"), ...when(!off, stateNow) },
    "sep",
    { id: "clock", label: "Sync guest clock", run: () => h.clock("clock"), ...when(running && agent, !running ? stateNow : agentWhy) },
  ];
  const settings: MenuEntry[] = hardwareAreas.map(([label, area]) => ({ id: "edit:" + area, label, run: () => h.edit("edit:" + area, label, area) }));
  const snapshots: MenuEntry[] = [
    { id: "snapshot", label: "Take snapshot", run: () => h.takeSnapshot("snapshot"), disabled: !op, title: need },
    { id: "snapshots", label: "Manage snapshots", run: () => h.snapshots("snapshots") },
    { id: "schedule", label: "Schedule", run: () => h.schedule("schedule") },
  ];
  const canConvert = off || (running && agent);
  const movable = (vm.disks ?? []).filter((k) => k.device === "disk" && k.source && !k.source.startsWith("/dev/"));
  const migrate: MenuEntry[] = [
    { id: "migrate", label: "Migrate to another node", run: () => h.migrate("migrate"), disabled: !op, title: need },
    // One entry per disk that can be moved; it opens the move dialog itself.
    ...(movable.length === 0
      ? [{ id: "move-disk", label: "Move a disk to another pool", run: () => {}, disabled: true, title: op ? (vm.disks ? "This VM has no disk that can be moved" : "Checking the disks") : need }]
      : movable.map((k) => ({
          id: movable.length === 1 ? "move-disk" : "move-disk:" + k.target,
          label: movable.length === 1 ? "Move the disk to another pool" : `Move disk ${k.target} to another pool`,
          run: () => h.moveDisk(movable.length === 1 ? "move-disk" : "move-disk:" + k.target, k.target),
          disabled: !op,
          title: need,
        }))),
  ];
  const more: MenuEntry[] = [
    { id: "xml", label: "Edit XML", run: () => h.xml("xml") },
    "sep",
    { id: "delete", label: "Delete VM", danger: true, run: () => h.remove("delete"), ...when(off, "Shut the VM down first") },
  ];

  // The old copy after a failover can only be deleted (everything else is refused).
  if (vm.state === "stale") return [{ id: "delete", label: "Delete VM", danger: true, run: () => h.remove("delete"), disabled: !op, title: need }];
  return [
    { label: "Power", items: power, disabled: !op, title: need },
    { label: "Edit Settings", items: settings, disabled: !op, title: need },
    { label: "Snapshots", items: snapshots },
    { id: "clone", label: "Clone", run: () => h.clone("clone"), ...when(off, "Shut the VM down to clone it") },
    vm.template
      ? { id: "convert-back", label: "Convert back to a VM", run: () => h.convertBack("convert-back"), ...when(off, "Shut the VM down first") }
      : { id: "convert", label: "Convert to template", run: () => h.convert("convert"), ...when(canConvert, vm.agent === undefined && running ? loading : "Shut the VM down first, or start it with the guest agent connected") },
    { label: "Migrate", items: migrate, disabled: !op, title: need },
    { id: "replication", label: "Replication", run: () => h.replication("replication"), disabled: vm.template, title: vm.template ? "A template cannot be replicated" : undefined },
    { label: "More", items: more },
  ];
}
