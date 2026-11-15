import { useSearchParams } from "react-router-dom";
import { get, Task } from "../api";
import { useLive } from "../live";
import { useInventory } from "../inventory";
import TaskDetail from "../TaskDetail";
import TaskTable, { useNow } from "../TaskTable";
import { orderTasks, useStopTask } from "../taskView";
import { ErrorBox, PageTitle } from "../ui";

export default function Tasks() {
  const inv = useInventory();
  const stopper = useStopTask();
  // ?task=<id> opens that task's detail (the links of the toasts).
  const [params, setParams] = useSearchParams();
  const detail = Number(params.get("task")) || 0;
  const setDetail = (id: number) => setParams(id ? { task: String(id) } : {}, { replace: true });
  const tasks = useLive(() => get<Task[]>("/tasks"), "tasks", (e) => e.type === "task" || e.detail === "resync");
  const list = orderTasks(tasks.data ?? []);
  const now = useNow(list.some((t) => t.status === "running"));
  const shown = detail ? list.find((t) => t.id === detail) : undefined;
  return (
    <>
      <div className="head">
        <PageTitle>Tasks</PageTitle>
      </div>
      <ErrorBox error={tasks.error} />
      {tasks.data && tasks.data.length === 0 ? (
        <div className="empty">No tasks</div>
      ) : (
        <div className="taskpage">
          <TaskTable tasks={list} inv={inv} now={now} open={setDetail} stopping={(id) => stopper.asked.has(id)} stop={stopper.stop} label="Tasks" />
        </div>
      )}
      {shown && <TaskDetail task={shown} inv={inv} now={now} stopping={stopper.asked.has(shown.id)} stop={stopper.stop} onClose={() => setDetail(0)} />}
      {stopper.dialog}
    </>
  );
}
