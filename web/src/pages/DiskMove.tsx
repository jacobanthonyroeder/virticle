import { useEffect, useState } from "react";
import { CheckList, preflightItems } from "../Checks";
import { bytes, get, Pool, post, VMDetail } from "../api";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";
import DialogAdvanced from "./DialogAdvanced";
import PoolSelect from "../PoolSelect";

interface CheckT {
  check: string;
  ok: boolean;
  severity: "error" | "warning" | "info";
  message: string;
}
interface Plan {
  ok: boolean;
  mode?: "live" | "offline";
  checks: CheckT[];
  source?: string;
  format?: string;
  capacity: number;
  allocation: number;
  backing: boolean;
  newPath?: string;
}

type DiskT = VMDetail["disks"][number];


/** Move one disk to another storage pool of the same node: pre-flight and options. Once the server accepted the move the dialog closes; the Tasks dock follows it and stops it. */
export default function DiskMoveDialog({ d, disk, onClose }: { d: VMDetail; disk: DiskT; onClose: () => void }) {
  const base = `/nodes/${d.node}/vms/${d.uuid}/disks/${disk.target}/move`;
  const oldName = (disk.source ?? "").split("/").pop() ?? "";
  const [pools, setPools] = useState<Pool[]>([]);
  const [pool, setPool] = useState("");
  const [flatten, setFlatten] = useState(false);
  const [answer, setAnswer] = useState<{ key: string; p: Plan }>();
  const act = useAction();
  const cancel = useAction();
  const confirm = useConfirm();

  useEffect(() => {
    get<Pool[]>(`/nodes/${d.node}/pools`).then((ps) => {
      const ok = ps.filter((p) => p.state === "running");
      setPools(ok);
      const cur = ok.find((p) => oldName && disk.source?.startsWith((p.target ?? "\0").replace(/\/?$/, "/")));
      const first = ok.find((p) => p.name !== cur?.name);
      if (first) setPool(first.name);
    }, () => {});
  }, [d.node]);

  // Pre-flight whenever the pool or the copy option changes (debounced, one request).
  useEffect(() => {
    let stale = false;
    const key = `${pool}|${flatten}`;
    const h = setTimeout(() => {
      const q = new URLSearchParams({ pool, flatten: flatten ? "1" : "0" });
      get<Plan>(`${base}/plan?${q}`).then(
        (p) => !stale && setAnswer({ key, p }),
        (e) => !stale && setAnswer({ key, p: { ok: false, checks: [{ check: "Pre-flight", ok: false, severity: "error", message: String(e.message ?? e) }], capacity: 0, allocation: 0, backing: false } }),
      );
    }, 300);
    return () => {
      stale = true;
      clearTimeout(h);
    };
  }, [pool, flatten, base]);

  const start = async () => {
    const ok = await confirm.ask({
      title: `Move ${disk.target} and delete the old volume`,
      message: `${disk.target} of ${d.name} is copied to pool ${pool}. When the copy has finished, the old volume ${oldName} is deleted and cannot be recovered.`,
      confirm: "Move and delete old volume",
    });
    if (!ok) return;
    if (await act.run(() => post(base, { pool, flatten }))) onClose();
  };
  // The answer counts only for the pool and option it was asked for.
  const plan = answer?.key === `${pool}|${flatten}` ? answer.p : undefined;
  const checking = !plan;
  const live = plan?.mode === "live";

  return (
    <Modal title={`Move ${disk.target} to another pool`} onClose={onClose} wide>
      {confirm.dialog}
      <p className="muted">
        <span className="mono wrap">{disk.source}</span>
        {disk.capacity ? ` · ${bytes(disk.capacity)}` : ""}
      </p>
      <div className="row">
        <Field label="Target pool" group>
          <PoolSelect pools={pools} value={pool} onChange={setPool} label="Target pool" needBytes={disk.capacity} />
        </Field>
      </div>
      {plan?.backing && (
        <DialogAdvanced>
          <label className="check">
            <input type="checkbox" checked={flatten} onChange={(e) => setFlatten(e.target.checked)} />
            Copy as a standalone image (breaks the link to the base)
          </label>
        </DialogAdvanced>
      )}
      <h3>Pre-flight checks</h3>
      {checking && <p className="muted">Checking</p>}
      {plan && (
        <>
          {plan.mode && (
            <p>
              <strong>{live ? "Live: the VM keeps running." : "Offline: the VM is shut off."}</strong>
              {plan.newPath && (
                <>
                  {" "}
                  New file: <span className="mono wrap">{plan.newPath}</span>
                </>
              )}
            </p>
          )}
          <CheckList items={preflightItems(plan.checks)} />
          {plan.checks.some((c) => !c.ok && c.message.includes("a move of this disk is already in progress")) && (
            <p>
              <button
                className="danger"
                disabled={cancel.busy}
                onClick={async () =>
                  (await confirm.ask({ title: "Stop disk move", message: `The running move of ${disk.target} of ${d.name} is stopped. The disk stays where it was.`, confirm: "Stop move" })) &&
                  cancel.run(async () => {
                    await post(`${base}/cancel`);
                    onClose();
                  })
                }
              >
                Cancel the running move
              </button>
            </p>
          )}
          <ErrorBox error={cancel.error} />
        </>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || checking || !plan?.ok || !pool} onClick={start}>
          Move disk
        </button>
      </footer>
    </Modal>
  );
}
