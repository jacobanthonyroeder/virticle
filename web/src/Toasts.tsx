// Toasts: what needs a look right now, wherever the user is, stacked above the Tasks dock.
//  - A failed task started by the signed-in user and run in the background stays until it is dismissed, with links to the
//    task and its target (one that the request waited for shows 10 s: its error is in the page that asked).
//  - A failed task of anyone else (or of the scheduler) shows for 10 s, for operators and administrators.
//  - A newly raised alert shows with its severity: an error stays until dismissed, a warning for 10 s. The alert of a
//    failed task has no toast of its own: the task's toast above already tells it (once).
// Toasts come from the live event stream only: one that was missed while the page was closed is not replayed; the
// alert bell and the Tasks dock still hold it. No animation: a toast appears and disappears.
import { X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Alert, Role, Task } from "./api";
import { atLeast } from "./auth";
import { taskLinks, useInventory } from "./inventory";
import { useEvents } from "./live";
import { openAlerts } from "./pages/Alerts";

const SHOWN = 4; // toasts visible at once; the rest are counted
const KEPT = 20; // toasts held
const TIMED_MS = 10000;

type Toast =
  | { key: string; sticky: boolean; severity: "error"; kind: "task"; task: Task }
  | { key: string; sticky: boolean; severity: "error" | "warning"; kind: "alert"; alert: Alert };

export default function Toasts({ user, role }: { user: string; role: Role }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const timers = useRef(new Map<string, number>());
  const inv = useInventory();

  const dismiss = useCallback((key: string) => {
    window.clearTimeout(timers.current.get(key));
    timers.current.delete(key);
    setToasts((l) => l.filter((x) => x.key !== key));
  }, []);
  const add = useCallback(
    (t: Toast) => {
      window.clearTimeout(timers.current.get(t.key));
      if (!t.sticky) timers.current.set(t.key, window.setTimeout(() => dismiss(t.key), TIMED_MS));
      setToasts((l) => [...l.filter((x) => x.key !== t.key), t].slice(-KEPT));
    },
    [dismiss],
  );
  useEffect(() => {
    const m = timers.current;
    return () => m.forEach((id) => window.clearTimeout(id));
  }, []);

  useEvents((e) => {
    if (e.type === "task" && e.task?.status === "failed") {
      const own = !!user && e.task.user === user;
      // A task that ran while the request waited has its error in the page that asked (a dialog); only one that ran on
      // after the request was answered needs a message that stays.
      if (own || atLeast(role, "operator")) add({ key: "task:" + e.task.id, sticky: own && !!e.task.background, severity: "error", kind: "task", task: e.task });
    } else if (e.type === "alert" && e.detail === "raised" && e.alert) {
      const a = e.alert;
      if (a.kind === "failed-job") return; // the failed task has its own toast
      const error = a.severity === "critical";
      add({ key: "alert:" + a.id, sticky: error, severity: error ? "error" : "warning", kind: "alert", alert: a });
    }
  });

  const shown = toasts.slice(-SHOWN);
  const more = toasts.length - shown.length;
  return (
    <div className="toasts">
      {more > 0 && (
        <button type="button" className="toast-more" onClick={openAlerts}>
          {more} more
        </button>
      )}
      {shown.map((t) => {
        const error = t.severity === "error";
        return (
          <div key={t.key} className={"toast " + t.severity} role={error ? "alert" : "status"} aria-live={error ? "assertive" : "polite"}>
            <div className="toast-body">
              {t.kind === "task" ? <TaskToast task={t.task} inv={inv} /> : <AlertToast alert={t.alert} />}
            </div>
            <button type="button" className="toast-x" aria-label="Dismiss" title="Dismiss" onClick={() => dismiss(t.key)}>
              <X size={14} aria-hidden />
            </button>
          </div>
        );
      })}
    </div>
  );
}

function TaskToast({ task, inv }: { task: Task; inv: ReturnType<typeof useInventory> }) {
  const to = taskLinks(inv, task.node, task.target).target;
  return (
    <>
      <strong>
        {task.kind} {task.target} failed
      </strong>
      <div className="toast-text">{task.error}</div>
      <div className="toast-links">
        <Link to={`/datacenter/tasks?task=${task.id}`}>Task</Link>
        {to && <Link to={to}>Target</Link>}
      </div>
    </>
  );
}

function AlertToast({ alert }: { alert: Alert }) {
  return (
    <>
      <strong>{alert.title}</strong>
      <div className="toast-text">{alert.message}</div>
      <div className="toast-links">
        <button type="button" className="link" onClick={openAlerts}>
          Open alerts
        </button>
      </div>
    </>
  );
}
