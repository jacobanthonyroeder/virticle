// The detail of one task: every field in full, with the whole Result text. Opened from a row of the Tasks dock or
// the Tasks page.
import { ReactNode } from "react";
import { Task } from "./api";
import { Inventory } from "./inventory";
import { resultText } from "./TaskTable";
import { absTime, duration, StopTask, TaskNode, TaskStatus, TaskTarget } from "./taskView";
import { Modal } from "./ui";
import { atLeast, useRole } from "./auth";
import OperationList from "./OperationList";

export default function TaskDetail({ task, inv, now, stopping, stop, trace, onClose }: { task: Task; inv: Inventory; now: number; stopping: boolean; stop: (t: Task) => void; trace?: ReactNode; onClose: () => void }) {
  const text = resultText(task);
  const admin = atLeast(useRole(), "admin");
  return (
    <Modal title={`${task.kind} ${task.target}`} onClose={onClose} wide={admin && !!task.ops}>
      <dl className="tdetail">
        <dt>Status</dt>
        <dd>
          <TaskStatus status={task.status} />
          {task.status === "running" && task.progress > 0 && <span className="num"> {(task.progress * 100).toFixed(0)}%</span>}
          <StopTask task={task} asked={stopping} onStop={stop} />
        </dd>
        <dt>Action</dt>
        <dd>{task.kind}</dd>
        <dt>Target</dt>
        <dd>
          <TaskTarget task={task} inv={inv} />
        </dd>
        <dt>Node</dt>
        <dd>{task.node ? <TaskNode task={task} inv={inv} /> : ""}</dd>
        <dt>By</dt>
        <dd>{task.user}</dd>
        <dt>Started</dt>
        <dd>{absTime(task.started)}</dd>
        {task.finished ? (
          <>
            <dt>Finished</dt>
            <dd>{absTime(task.finished)}</dd>
          </>
        ) : null}
        <dt>Duration</dt>
        <dd>{duration(task, now)}</dd>
        <dt>Result</dt>
        <dd className="tdetail-result">{text}</dd>
      </dl>
      {/* What the task ran, for administrators (as the audit log). */}
      {trace ?? (admin && !!task.ops && <OperationList task={task.id} count={task.ops} />)}
      <footer>
        <button className="primary" autoFocus onClick={onClose}>
          Close
        </button>
      </footer>
    </Modal>
  );
}
