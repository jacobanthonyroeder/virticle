// The task table of the Tasks dock and the Tasks page: a grid whose cells never wrap. Text longer than its cell
// is cut at the cell's edge (no ellipsis); the full text is the cell's title and is in the task's detail. A grip on
// the right edge of each column head resizes it: drag, arrow keys on the focused grip, double click for the default.
import { CSSProperties, memo, RefObject, useEffect, useRef, useState } from "react";
import { Task } from "./api";
import { StatusDot } from "./Checks";
import { Inventory } from "./inventory";
import { COLS, ColDef, COL_MAX, RESULT_MIN, getWidths, resetWidth, setWidth, template, useWidths, widthOf } from "./taskCols";
import { absTime, duration, relTime, StopTask, TaskNode, TaskStatus, TaskTarget } from "./taskView";
import "./tasksdock.css";

/** The time in ms, rounded down to `ms`, so rows that did not change are not redrawn. Counts only while `active`. */
export function useNow(active: boolean, ms = 5000): number {
  const coarse = () => Math.floor(Date.now() / ms) * ms;
  const [now, setNow] = useState(coarse);
  useEffect(() => {
    if (!active) return;
    setNow(coarse());
    const id = window.setInterval(() => document.visibilityState !== "hidden" && setNow(coarse()), ms);
    return () => window.clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, ms]);
  return now;
}

/** The text of the Result column: the error of a failed task, else the fact of a finished one. */
export const resultText = (t: Task) => t.error || t.note || "";

interface RowProps {
  t: Task;
  inv: Inventory;
  now: number;
  open: (id: number) => void;
  stopping: boolean;
  stop: (t: Task) => void;
}

const Row = memo(function Row({ t, inv, now, open, stopping, stop }: RowProps) {
  const text = resultText(t);
  return (
    <div
      className="dk-row"
      role="row"
      data-status={t.status}
      onClick={(e) => {
        if (!(e.target as HTMLElement).closest("a, button")) open(t.id);
      }}
    >
      <div role="cell" className="dk-cell c-status">
        <TaskStatus status={t.status} />
      </div>
      <div role="cell" className="dk-cell c-kind" title={t.kind}>
        {t.kind}
      </div>
      <div role="cell" className="dk-cell c-target" title={t.target}>
        <TaskTarget task={t} inv={inv} />
      </div>
      <div role="cell" className="dk-cell c-node" title={t.node}>
        <TaskNode task={t} inv={inv} />
      </div>
      <div role="cell" className="dk-cell c-user" title={t.user}>
        {t.user}
      </div>
      <div role="cell" className="dk-cell c-when num" title={absTime(t.started)}>
        {relTime(t.started, now)}
      </div>
      <div role="cell" className="dk-cell c-dur num">
        {duration(t, now)}
      </div>
      <div role="cell" className="dk-cell c-res">
        {t.status === "running" ? (
          <span className="dk-prog" title={t.progress > 0 ? `${(t.progress * 100).toFixed(0)}%` : "no progress reported"}>
            <span className="dk-bar">
              <span style={{ width: `${Math.round(Math.min(1, t.progress) * 100)}%` }} />
            </span>
            <span className="num">{t.progress > 0 ? `${(t.progress * 100).toFixed(0)}%` : ""}</span>
            <StopTask task={t} asked={stopping} onStop={stop} />
          </span>
        ) : text ? (
          // Not a button: its text would match every search for a button of that name on the page. The row opens the
          // detail by click; the focus and Enter do the same for the keyboard.
          <span
            className="dk-err"
            title={text}
            tabIndex={0}
            onClick={() => open(t.id)}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                open(t.id);
              }
            }}
          >
            <StatusDot status={t.error ? "error" : "info"} />
            {text}
          </span>
        ) : null}
      </div>
    </div>
  );
});

const STEP = 8;

/** The grip on the right edge of a column head. */
function Grip({ col, table }: { col: ColDef; table: RefObject<HTMLDivElement> }) {
  const drag = useRef<{ x: number; w: number; last: number } | null>(null);
  const shown = (el: HTMLElement) => Math.round(el.parentElement!.getBoundingClientRect().width);
  // The widest this column may get while Result keeps its minimum.
  const most = () => {
    const total = table.current?.clientWidth ?? 0;
    const others = COLS.reduce((s, c) => s + (c.id === col.id ? 0 : widthOf(c)), 0);
    return Math.max(col.min, Math.min(COL_MAX, total - others - RESULT_MIN));
  };
  const clamp = (v: number) => Math.round(Math.max(col.min, Math.min(most(), v)));
  const preview = (v: number) => table.current?.style.setProperty("--dk-cols", template({ ...getWidths(), [col.id]: v }));
  return (
    <div
      className="dk-grip"
      role="separator"
      tabIndex={0}
      aria-orientation="vertical"
      aria-label={`Resize ${col.label} column`}
      aria-valuenow={widthOf(col)}
      aria-valuemin={col.min}
      aria-valuemax={COL_MAX}
      onPointerDown={(e) => {
        if (e.button !== 0) return;
        e.preventDefault();
        e.currentTarget.setPointerCapture(e.pointerId);
        const w = shown(e.currentTarget);
        drag.current = { x: e.clientX, w, last: w };
        document.body.classList.add("resizing-x");
      }}
      onPointerMove={(e) => {
        const d = drag.current;
        if (!d) return;
        d.last = clamp(d.w + e.clientX - d.x);
        preview(d.last);
      }}
      onPointerUp={(e) => {
        const d = drag.current;
        if (!d) return;
        drag.current = null;
        e.currentTarget.releasePointerCapture(e.pointerId);
        document.body.classList.remove("resizing-x");
        if (d.last !== d.w) setWidth(col, d.last);
      }}
      onPointerCancel={() => {
        drag.current = null;
        document.body.classList.remove("resizing-x");
        table.current?.style.setProperty("--dk-cols", template(getWidths()));
      }}
      onDoubleClick={() => resetWidth(col)}
      onKeyDown={(e) => {
        const step = e.shiftKey ? STEP * 4 : STEP;
        if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
          e.preventDefault();
          setWidth(col, clamp(shown(e.currentTarget) + (e.key === "ArrowRight" ? step : -step)));
        } else if (e.key === "Home") {
          e.preventDefault();
          resetWidth(col);
        }
      }}
    />
  );
}

export interface TaskTableProps {
  tasks: Task[];
  inv: Inventory;
  now: number;
  open: (id: number) => void;
  stopping: (id: number) => boolean;
  stop: (t: Task) => void;
  label?: string;
}

export default function TaskTable({ tasks, inv, now, open, stopping, stop, label = "Recent tasks" }: TaskTableProps) {
  const table = useRef<HTMLDivElement>(null);
  const widths = useWidths();
  return (
    <div className="dock-table" role="table" aria-label={label} ref={table} style={{ "--dk-cols": template(widths) } as CSSProperties}>
      <div className="dk-head" role="rowgroup">
        <div className="dk-row" role="row">
          {COLS.map((c) => (
            <div key={c.id} role="columnheader" className={"dk-cell c-" + c.id}>
              {c.label}
              <Grip col={c} table={table} />
            </div>
          ))}
          <div role="columnheader" className="dk-cell c-res">
            Result
          </div>
        </div>
      </div>
      <div className="dk-rows" role="rowgroup">
        {tasks.map((t) => (
          <Row key={t.id} t={t} inv={inv} now={now} open={open} stopping={stopping(t.id)} stop={stop} />
        ))}
      </div>
    </div>
  );
}
