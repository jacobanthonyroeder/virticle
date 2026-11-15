// The "Actions" menu of the VM page. The menu itself is vmMenu() (../vmMenu.ts), shared with the tree's
// right-click menu; this file gives it handlers that open the page's dialogs. Destructive power actions
// ask for confirmation first.
import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { del, post, put, VMDetail } from "../api";
import { atLeast, useRole } from "../auth";
import { DropdownMenu } from "../Menu";
import { powerConfirm } from "../power";
import { findEntry, vmMenu } from "../vmMenu";
import { ErrorBox, Modal, useAction, useConfirm } from "../ui";
import { CloneDialog, ConvertDialog } from "./Clone";
import MigrateDialog from "./Migrate";
import DiskMoveDialog from "./DiskMove";
import { HardwareDialog, HwArea } from "./Hardware";
import { TakeSnapshot } from "./Snapshots";

export default function VMActions(props: {
  d: VMDetail;
  base: string;
  /** Switch tab, then scroll the element with this id into view. */
  goto: (tab: string, card?: string) => void;
  onChanged: () => void;
  /** Runs a call with the page's busy and error state. */
  run: (fn: () => Promise<unknown>) => Promise<boolean>;
  busy: boolean;
  openReplication: () => void;
}) {
  const { d, base, goto, run, busy } = props;
  const nav = useNavigate();
  const op = atLeast(useRole(), "operator");
  const confirm = useConfirm();
  const [cloning, setCloning] = useState(false);
  const [converting, setConverting] = useState(false);
  const [taking, setTaking] = useState(false);
  const [migrating, setMigrating] = useState(false);
  const [movingDisk, setMovingDisk] = useState<string>();
  const [deleting, setDeleting] = useState(false);
  const [editing, setEditing] = useState<[string, HwArea]>();

  const act = async (a: string, body: { action: string } = { action: a }) => {
    const spec = powerConfirm(a, [d.name]);
    if (spec && !(await confirm.ask(spec))) return;
    run(() => post(`${base}/action`, body));
  };
  const moveDisk = d.disks.find((k) => k.target === movingDisk);
  const items = vmMenu(
    { vm: d, op, busy },
    {
      power: (_, a) => act(a),
      clock: () => run(() => post(`${base}/clock/sync`)),
      edit: (_, label, area) => setEditing([label, area]),
      takeSnapshot: () => setTaking(true),
      snapshots: () => goto("snapshots"),
      schedule: () => goto("snapshots", "snap-schedule"),
      clone: () => setCloning(true),
      convert: () => setConverting(true),
      convertBack: () => run(async () => { await put(`${base}/template`, { template: false }); props.onChanged(); }),
      migrate: () => setMigrating(true),
      moveDisk: (_, target) => setMovingDisk(target),
      replication: props.openReplication,
      xml: () => goto("xml"),
      remove: () => setDeleting(true),
    },
  );

  // "?do=<entry id>" (from the tree's menu) runs that entry of this menu on arrival, if it is enabled now.
  const [params, setParams] = useSearchParams();
  const todo = params.get("do");
  useEffect(() => {
    if (!todo) return;
    const next = new URLSearchParams(params);
    next.delete("do");
    setParams(next, { replace: true });
    const e = findEntry(items, todo);
    // After the address changed: an entry that goes to a tab sets the address again.
    if (e && !e.disabled) setTimeout(() => e.run?.(), 0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [todo]);

  return (
    <>
      <DropdownMenu label="Actions" items={items} className="primary" />
      {confirm.dialog}
      {editing && <HardwareDialog area={editing[1]} title={editing[0]} d={d} base={base} onChanged={props.onChanged} onClose={() => setEditing(undefined)} />}
      {cloning && <CloneDialog d={d} base={base} onClose={() => setCloning(false)} />}
      {converting && <ConvertDialog d={d} base={base} onClose={() => setConverting(false)} onDone={props.onChanged} />}
      {taking && <TakeSnapshot d={d} base={base} onClose={() => setTaking(false)} onDone={props.onChanged} />}
      {migrating && <MigrateDialog d={d} onClose={() => setMigrating(false)} />}
      {moveDisk && <DiskMoveDialog d={d} disk={moveDisk} onClose={() => setMovingDisk(undefined)} />}
      {deleting && <DeleteVM d={d} base={base} onClose={() => setDeleting(false)} onDeleted={() => nav("/datacenter/vms")} />}
    </>
  );
}

function DeleteVM(props: { d: VMDetail; base: string; onClose: () => void; onDeleted: () => void }) {
  const { d } = props;
  const [disks, setDisks] = useState(true);
  const act = useAction();
  const [leftDisks, setLeftDisks] = useState<string[]>();
  const [replicaNote, setReplicaNote] = useState<string>();
  const [replica, setReplica] = useState(true);
  const owned = d.disks.filter((k) => k.device === "disk" && !k.readOnly && k.source);
  if (leftDisks || replicaNote)
    return (
      <Modal title={`${d.name} was deleted`} onClose={props.onDeleted}>
        {replicaNote && <p>The replica on {d.replication?.targetName} was not deleted: {replicaNote}</p>}
        {leftDisks && leftDisks.length > 0 && (
          <>
            <p>These disks are not in any storage pool and were left in place:</p>
            <p className="mono wrap">{leftDisks.join(", ")}</p>
          </>
        )}
        <footer>
          <button autoFocus onClick={props.onDeleted}>Close</button>
        </footer>
      </Modal>
    );
  return (
    <Modal title={`Delete ${d.name}?`} onClose={props.onClose}>
      <p>The VM's definition{d.snapshots.length ? `, its ${d.snapshots.length} snapshot record(s)` : ""} and firmware settings are removed. This cannot be undone.</p>
      {owned.length > 0 && (
        <label className="check">
          <input type="checkbox" checked={disks} onChange={(e) => setDisks(e.target.checked)} />
          Also delete its disk{owned.length > 1 ? "s" : ""}: <span className="mono">{owned.map((k) => k.source).join(", ")}</span>
        </label>
      )}
      {d.replication && (
        <label className="check">
          <input type="checkbox" checked={replica} onChange={(e) => setReplica(e.target.checked)} />
          Also delete the replica on {d.replication.targetName}
        </label>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button autoFocus onClick={props.onClose}>Cancel</button>
        <button className="danger" disabled={act.busy} onClick={async () => {
            let left: string[] = [];
            let note = "";
            const q = [...(disks ? ["disks=1"] : []), ...(d.replication && replica ? ["replica=1"] : [])].join("&");
            const ok = await act.run(async () => {
              const r = await del<{ leftDisks: string[]; replicaNote?: string }>(`${props.base}${q ? "?" + q : ""}`);
              left = r.leftDisks ?? [];
              note = r.replicaNote ?? "";
            });
            if (!ok) return;
            if (left.length || note) {
              setLeftDisks(left);
              setReplicaNote(note || undefined);
            } else props.onDeleted();
          }}>
          Delete VM
        </button>
      </footer>
    </Modal>
  );
}
