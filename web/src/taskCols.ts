// Column widths of the task table (dock and Tasks page). Kept in this browser only; the Result column takes the
// space that is left, so it has no width of its own.
import { useSyncExternalStore } from "react";

export interface ColDef {
  id: string;
  label: string;
  /** Default width in px, sized for the usual content of the column. */
  w: number;
  /** Narrowest the column gets, by dragging or when the window is narrow. */
  min: number;
}

export const COLS: ColDef[] = [
  { id: "status", label: "Status", w: 84, min: 56 },
  { id: "kind", label: "Action", w: 132, min: 48 },
  { id: "target", label: "Target", w: 172, min: 48 },
  { id: "node", label: "Node", w: 100, min: 48 },
  { id: "user", label: "By", w: 72, min: 36 },
  { id: "when", label: "Started", w: 88, min: 48 },
  { id: "dur", label: "Duration", w: 92, min: 48 },
];

/** The Result column never gets narrower than this when a column is dragged wider. */
export const RESULT_MIN = 120;
export const COL_MAX = 640;

const KEY = "virticle-task-cols";
export type Widths = Record<string, number>;

function read(): Widths {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? "{}");
    const out: Widths = {};
    for (const c of COLS) if (typeof v?.[c.id] === "number" && v[c.id] >= c.min && v[c.id] <= COL_MAX) out[c.id] = Math.round(v[c.id]);
    return out;
  } catch {
    return {};
  }
}

let widths: Widths = read();
const subs = new Set<() => void>();

function save(next: Widths) {
  widths = next;
  try {
    if (Object.keys(next).length) localStorage.setItem(KEY, JSON.stringify(next));
    else localStorage.removeItem(KEY);
  } catch {
    /* storage blocked: the widths last for this page */
  }
  subs.forEach((f) => f());
}

export const getWidths = () => widths;
export const widthOf = (c: ColDef, w: Widths = widths) => w[c.id] ?? c.w;

export function setWidth(c: ColDef, w: number) {
  const v = Math.round(Math.max(c.min, Math.min(COL_MAX, w)));
  if (v === c.w) resetWidth(c);
  else save({ ...widths, [c.id]: v });
}

export function resetWidth(c: ColDef) {
  const { [c.id]: _gone, ...rest } = widths;
  save(rest);
}

export function useWidths(): Widths {
  return useSyncExternalStore(
    (f) => {
      subs.add(f);
      return () => subs.delete(f);
    },
    () => widths,
  );
}

/**
 * The grid columns of every row: each column is as wide as set, shrinks towards its minimum when the window is
 * narrow, and Result takes the rest. Nothing runs off the side.
 */
export function template(w: Widths): string {
  return (
    COLS.map((c) => {
      const v = widthOf(c, w);
      return `minmax(${Math.min(c.min, v)}px, ${v}px)`;
    }).join(" ") + ` minmax(${RESULT_MIN}px, 1fr)`
  );
}
