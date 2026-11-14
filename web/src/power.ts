// Power actions: one set of labels and confirmation texts for the VM page, the tree and the lists.
import type { ConfirmSpec } from "./ui";

export type PowerAction = "start" | "shutdown" | "poweroff" | "reboot" | "reset" | "pause" | "resume";

export const powerLabel: Record<PowerAction, string> = {
  start: "Power On",
  shutdown: "Shutdown (graceful)",
  poweroff: "Halt (Force Shutdown)",
  reboot: "Reboot (Graceful)",
  reset: "Reset (Hard Reboot)",
  pause: "Pause",
  resume: "Resume",
};

const what: Partial<Record<PowerAction, { title: string; confirm: string; one: (n: string) => string; many: string }>> = {
  shutdown: {
    title: "Shutdown (graceful)",
    confirm: "Shutdown",
    one: (n) => `${n} is asked to shut down through ACPI. A guest that ignores the request keeps running.`,
    many: "These VMs are asked to shut down through ACPI. A guest that ignores the request keeps running.",
  },
  reboot: {
    title: "Reboot (Graceful)",
    confirm: "Reboot",
    one: (n) => `${n} is asked to restart through ACPI. It is unavailable while it restarts.`,
    many: "These VMs are asked to restart through ACPI. They are unavailable while they restart.",
  },
  reset: {
    title: "Reset (Hard Reboot)",
    confirm: "Reset",
    one: (n) => `${n} is reset at once, like the hardware reset button. Unsaved data is lost.`,
    many: "These VMs are reset at once, like the hardware reset button. Unsaved data is lost.",
  },
  poweroff: {
    title: "Halt (Force Shutdown)",
    confirm: "Halt",
    one: (n) => `${n} is powered off at once, like pulling the plug. Unsaved data in the guest is lost and its disks may need a repair at the next start.`,
    many: "These VMs are powered off at once, like pulling the plug. Unsaved data in the guests is lost.",
  },
};

/** The confirmation for a power action, or undefined for actions that are not destructive. */
export function powerConfirm(a: string, names: string[]): ConfirmSpec | undefined {
  const w = what[a as PowerAction];
  if (!w || names.length === 0) return undefined;
  return names.length === 1
    ? { title: w.title, message: w.one(names[0]), confirm: w.confirm }
    : { title: `${w.title}: ${names.length} VMs`, message: w.many, confirm: w.confirm, items: names };
}
