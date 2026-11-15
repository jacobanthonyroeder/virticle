import { useEffect, useState } from "react";
import { Lock } from "lucide-react";
import { CheckList, preflightItems } from "../Checks";
import { bytes, get, NodeStatus, post, VMDetail } from "../api";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import DialogAdvanced from "./DialogAdvanced";

interface Check {
  check: string;
  ok: boolean;
  severity: "error" | "warning" | "info";
  message: string;
}
interface Preview {
  ok: boolean;
  mode?: "live" | "suspended" | "offline";
  checks: Check[];
  copyBytes: number;
  mirrorBytes: number;
  encrypt?: boolean;
}

const modeText = { live: "Live: the VM keeps running", suspended: "Non-live: the VM is paused during the copy", offline: "Offline: the VM is shut off" };


/** Move a VM to another node: pre-flight checks and options. Once the server accepted the migration the dialog closes; the Tasks dock follows it. */
export default function MigrateDialog(props: { d: VMDetail; onClose: () => void }) {
  const { d } = props;
  const [nodes, setNodes] = useState<NodeStatus[]>([]);
  const [target, setTarget] = useState(0);
  const [keep, setKeep] = useState(false);
  const [answer, setAnswer] = useState<{ target: number; p: Preview } | undefined>();
  const act = useAction();
  const base = `/nodes/${d.node}/vms/${d.uuid}`;

  useEffect(() => {
    get<{ nodes: NodeStatus[] }>("/nodes").then((r) => {
      const others = r.nodes.filter((n) => n.id !== d.node);
      setNodes(others);
      const first = others.find((n) => n.state === "online");
      if (first) setTarget(first.id);
    }, () => {});
  }, [d.node]);

  // Pre-flight whenever the target changes. A running VM is migrated live, a shut-off one offline; the
  // server decides that and encrypts when both nodes are set up for it.
  useEffect(() => {
    if (!target) return;
    let stale = false;
    setAnswer(undefined);
    post<Preview>(`${base}/migrate/check`, { target }).then(
      (p) => !stale && setAnswer({ target, p }),
      (e) => !stale && setAnswer({ target, p: { ok: false, checks: [{ check: "Pre-flight", ok: false, severity: "error", message: String(e.message ?? e) }], copyBytes: 0, mirrorBytes: 0 } }),
    );
    return () => {
      stale = true;
    };
  }, [target, base]);

  const start = async () => {
    if (await act.run(() => post(`${base}/migrate`, { target, keepSourceDisks: keep }))) props.onClose();
  };

  // The answer counts only for the target it was asked for, so the button is off from the moment the target changes.
  const preview = answer?.target === target ? answer.p : undefined;
  const checking = !!target && !preview;
  const blocked = !preview || !preview.ok;
  const targetName = nodes.find((n) => n.id === target)?.hostname ?? "";

  return (
    <Modal title={`Migrate ${d.name}`} onClose={props.onClose} wide>
      <div className="row">
        <Field label="Target node">
          <Select
            label="Target node"
            value={String(target)}
            onChange={(v) => setTarget(Number(v))}
            placeholder="no other node"
            options={nodes.map((n) => ({ value: String(n.id), label: (n.hostname || n.addr) + (n.state !== "online" ? ` (${n.state})` : ""), disabled: n.state !== "online", dot: "n-" + n.state }))}
          />
        </Field>
      </div>
      <DialogAdvanced>
        <label className="check">
          <input type="checkbox" checked={keep} onChange={(e) => setKeep(e.target.checked)} />
          Keep the disks on the source node
        </label>
      </DialogAdvanced>
      <h3>Pre-flight checks</h3>
      {checking && <p className="muted">Checking {targetName}</p>}
      {preview && (
        <>
          {preview.mode && (
            <p>
              <strong>{modeText[preview.mode]}.</strong>{" "}
              {preview.mode !== "offline" && (
                <span className={`badge ${preview.encrypt ? "s-ok" : "s-paused"}`} title={preview.encrypt ? "Memory and disk data are TLS-encrypted on the wire" : "Memory and disk data cross the network unencrypted"}>
                  {preview.encrypt ? <><Lock size={12} aria-hidden="true" /> encrypted</> : "unencrypted"}
                </span>
              )}{" "}
              {preview.copyBytes > 0 && <>Files to copy: {bytes(preview.copyBytes)}. </>}
              {preview.mirrorBytes > 0 && <>Disk data mirrored while running: up to {bytes(preview.mirrorBytes)}.</>}
            </p>
          )}
          <CheckList items={preflightItems(preview.checks)} />
        </>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={props.onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || checking || blocked || !target} onClick={start}>
          Migrate
        </button>
      </footer>
    </Modal>
  );
}

