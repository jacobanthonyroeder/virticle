import { Fragment, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { get, Task } from "../api";
import { atLeast, useRole } from "../auth";
import OperationList from "../OperationList";
import { useLive } from "../live";
import { useInventory } from "../inventory";
import { absTime, orderTasks, StopTask, TaskNode, TaskStatus, TaskTarget, useStopTask } from "../taskView";
import { ErrorBox, PageTitle } from "../ui";
import "../tasksdock.css";
import { StatusLine } from "../Checks";

export default function Tasks() {
  const inv = useInventory();
  const stopper = useStopTask();
  const tasks = useLive(() => get<Task[]>("/tasks"), "tasks", (e) => e.type === "task" || e.detail === "resync");
  const admin = atLeast(useRole(), "admin"); // the operations are the administrator's, like the audit log
  const [open, setOpen] = useState<Set<number>>(new Set());
  const toggle = (id: number) =>
    setOpen((o) => {
      const n = new Set(o);
      n.has(id) ? n.delete(id) : n.add(id);
      return n;
    });
  return (
    <>
      <div className="head">
        <PageTitle>Tasks</PageTitle>
      </div>
      <ErrorBox error={tasks.error} />
      {tasks.data && tasks.data.length === 0 ? (
        <div className="empty">No tasks</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Started</th>
              <th>Action</th>
              <th>Target</th>
              <th>Node</th>
              <th>By</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {orderTasks(tasks.data ?? []).map((t) => (
              <Fragment key={t.id}>
                <tr>
                  <td>{absTime(t.started)}</td>
                  <td>
                    {t.kind}
                    {admin && !!t.ops && (
                      <div>
                        <button type="button" className="row-open" aria-expanded={open.has(t.id)} onClick={() => toggle(t.id)}>
                          {open.has(t.id) ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
                          {t.ops === 1 ? "1 operation" : `${t.ops} operations`}
                        </button>
                      </div>
                    )}
                  </td>
                  <td>
                    <TaskTarget task={t} inv={inv} />
                  </td>
                  <td>
                    <TaskNode task={t} inv={inv} />
                  </td>
                  <td>{t.user}</td>
                  <td>
                    <TaskStatus status={t.status} />
                    {t.status === "running" && (
                      <span className="dk-prog">
                        <span className="dk-bar">
                          <span style={{ width: `${Math.round(Math.min(1, t.progress) * 100)}%` }} />
                        </span>
                        <span className="num">{t.progress > 0 ? `${(t.progress * 100).toFixed(0)}%` : ""}</span>
                        <StopTask task={t} asked={stopper.asked.has(t.id)} onStop={stopper.stop} />
                      </span>
                    )}
                    {t.error && <StatusLine status="error">{t.error}</StatusLine>}
                    {t.note && <StatusLine status="info">{t.note}</StatusLine>}
                  </td>
                </tr>
                {admin && open.has(t.id) && (
                  <tr className="ops-row">
                    <td colSpan={6}>
                      <OperationList task={t.id} count={t.ops} />
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
      {stopper.dialog}
    </>
  );
}
