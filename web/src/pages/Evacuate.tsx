import { useEffect, useState } from "react";
import { bytes, EvacPlan, NodeStatus, post } from "../api";
import DialogAdvanced from "./DialogAdvanced";
import { ErrorBox, Modal, StateBadge, useAction } from "../ui";
import { CheckList } from "../Checks";

/** Plan the evacuation of one node. Once the server accepted it the dialog closes; the Tasks dock follows the evacuation and its moves, and stops it. */
export default function Evacuate({ node, nodes, onClose }: { node: NodeStatus; nodes: NodeStatus[]; onClose: () => void }) {
  const [targets, setTargets] = useState<Record<number, boolean>>({});
  const [plan, setPlan] = useState<EvacPlan | undefined>();
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const check = useAction();
  const start = useAction();
  const others = nodes.filter((n) => n.id !== node.id);
  const eligible = (n: NodeStatus) => n.state === "online" && !n.maintenance;
  const chosen = others.filter((n) => eligible(n) && targets[n.id] !== false).map((n) => n.id);
  const chosenKey = chosen.join(",");
  const name = node.hostname || node.addr;

  const runCheck = async () => {
    setPlan(undefined);
    await check.run(async () => {
      const p = await post<EvacPlan>(`/nodes/${node.id}/evacuate/check`, { targets: chosen, includeShutOff: true });
      setPlan(p);
      const all: Record<string, boolean> = {};
      [...p.assignments.map((a) => a.vm), ...p.blocked.map((b) => b.vm)].forEach((v) => (all[v.uuid] = true));
      setPicked(all);
    });
  };

  // The check runs when the dialog opens and again when the choice of targets changes.
  useEffect(() => {
    if (chosen.length > 0) runCheck();
  }, [chosenKey]);

  const go = async () => {
    const vms = Object.keys(picked).filter((u) => picked[u]);
    if (await start.run(() => post(`/nodes/${node.id}/evacuate`, { targets: chosen, vms, includeShutOff: true }))) onClose();
  };

  return (
    <Modal title={`Evacuate ${name}`} onClose={onClose} wide>
      <p>Migrates the VMs to the other nodes; nodes in maintenance are not used.</p>
      {others.length === 0 && <div className="muted">No other node</div>}
      {others.length > 0 && chosen.length === 0 && <div className="muted">No target available</div>}
      {check.busy && !plan && <p className="muted">Checking</p>}
      <ErrorBox error={check.error} />
      {plan && (
        <div className="evacplan">
          <table className="compact">
            <thead>
              <tr>
                <th />
                <th>VM</th>
                <th>State</th>
                <th>Memory</th>
                <th>Goes to</th>
              </tr>
            </thead>
            <tbody>
              {plan.assignments.map((a) => (
                <tr key={a.vm.uuid}>
                  <td>
                    <input type="checkbox" checked={!!picked[a.vm.uuid]} onChange={(e) => setPicked({ ...picked, [a.vm.uuid]: e.target.checked })} aria-label={`Move ${a.vm.name}`} />
                  </td>
                  <td>{a.vm.name}</td>
                  <td>{a.vm.state}</td>
                  <td>{bytes(a.vm.memoryKiB * 1024)}</td>
                  <td>{a.targetName}</td>
                </tr>
              ))}
              {plan.blocked.map((b) => (
                <tr key={b.vm.uuid} className="blockedrow">
                  <td>
                    <span className="sev critical">Blocked</span>
                  </td>
                  <td>{b.vm.name}</td>
                  <td>{b.vm.state}</td>
                  <td>{bytes(b.vm.memoryKiB * 1024)}</td>
                  <td>
                    <CheckList summary={false} label="Why it is blocked" items={b.reasons.map((r) => ({ status: "error", detail: r }))} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {plan.assignments.length + plan.blocked.length === 0 && <div className="muted">No virtual machines</div>}
        </div>
      )}
      <DialogAdvanced>
        <strong>Targets</strong>
        {others.map((n) => (
          <label key={n.id} className="check">
            <input type="checkbox" checked={eligible(n) && targets[n.id] !== false} disabled={!eligible(n)} onChange={(e) => setTargets({ ...targets, [n.id]: e.target.checked })} />
            {n.hostname || n.addr}
            {n.state !== "online" && <StateBadge state={n.state} />}
            {n.maintenance && <span className="badge maint">Maintenance</span>}
          </label>
        ))}
      </DialogAdvanced>
      <ErrorBox error={start.error} />
      <footer>
        <button onClick={onClose}>Close</button>
        <button className="primary" onClick={go} disabled={start.busy || !plan || plan.assignments.every((a) => !picked[a.vm.uuid])}>
          Evacuate {plan ? plan.assignments.filter((a) => picked[a.vm.uuid]).length : 0} VM(s)
        </button>
      </footer>
    </Modal>
  );
}
