// What Virticle ran on the nodes for one audit entry or task: the libvirt calls that changed something (each with
// the virsh command that does the same) and the programs run on a node. GET /api/audit/{id}/ops and
// /api/tasks/{id}/ops (administrators). The list loads when it is shown and again when `count` changes, so a
// running task's list follows the task events; there is no polling.
import { useEffect, useState } from "react";
import { get } from "./api";
import DialogAdvanced from "./pages/DialogAdvanced";
import Help from "./Help";
import { ErrorBox } from "./ui";
import "./oplist.css";

export interface Operation {
  seq: number;
  time: number;
  node: string;
  kind: "libvirt" | "exec" | "limit";
  api: string;
  cmd?: string;
  args?: string;
  files?: { name: string; text: string }[];
  ok: boolean;
  error?: string;
  ms: number;
}

export function duration(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`;
  return `${Math.floor(ms / 60000)} min ${Math.round((ms % 60000) / 1000)} s`;
}

/** The node command is shown as "virticle-node VERB args": without sudo and its install path (the stored record keeps them). */
export function shortNodeCmd(cmd: string): string {
  return cmd.replace(/(?:sudo -n )?\/usr\/local\/libexec\/virticle-node(?=\s|$)/g, "virticle-node");
}

/** The text an operation line shows (and copies). */
export function opLine(o: Operation): string {
  if (o.cmd) return o.kind === "exec" ? shortNodeCmd(o.cmd) : o.cmd;
  return o.args ? `${o.api} (${o.args})` : o.api;
}

function copyText(text: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    ok ? resolve() : reject(new Error("The browser did not allow copying."));
  });
}

function OpRow({ o, nodes }: { o: Operation; nodes: boolean }) {
  const [copied, setCopied] = useState(false);
  if (o.kind === "limit") return <li className="op op-limit muted">Not recorded: more than 200 operations</li>;
  const text = opLine(o);
  const equiv = o.kind === "libvirt" && !!o.cmd;
  return (
    <li className="op">
      <div className="op-main">
        <code className="op-cmd">{text}</code>
        <span className="op-meta">
          {o.kind === "libvirt" && o.cmd && <span className="op-api">{o.api}</span>}
          {o.kind === "exec" && <span className="op-api">{o.api}</span>}
          {equiv && <span className="muted">equivalent</span>}
          {nodes && <span className="muted">{o.node}</span>}
          <span className={"badge " + (o.ok ? "s-ok" : "s-failed")}>{o.ok ? "OK" : "Failed"}</span>
          <span className="num muted">{duration(o.ms)}</span>
          <button
            type="button"
            className="link op-copy"
            aria-label={"Copy " + text}
            onClick={() =>
              copyText(text).then(
                () => setCopied(true),
                () => setCopied(false),
              )
            }
          >
            {copied ? "Copied" : "Copy"}
          </button>
        </span>
      </div>
      {o.error && <div className="op-err">{o.error}</div>}
      {o.files?.map((f) => (
        <DialogAdvanced key={f.name} flat title={<span className="mono">{f.name}</span>}>
          <pre className="op-xml">{f.text}</pre>
        </DialogAdvanced>
      ))}
    </li>
  );
}

/**
 * The operations of an audit entry (`audit`) or a task (`task`). `count`, when given, reloads the list when it
 * changes (the task list's `ops`).
 */
export default function OperationList({ audit, task, count }: { audit?: number; task?: number; count?: number }) {
  const path = audit !== undefined ? `/audit/${audit}/ops` : `/tasks/${task}/ops`;
  const [ops, setOps] = useState<Operation[] | undefined>();
  const [error, setError] = useState<string | undefined>();
  useEffect(() => {
    let live = true;
    get<{ ops: Operation[] }>(path).then(
      (r) => live && (setOps(r.ops), setError(undefined)),
      (e) => live && setError(String(e.message ?? e)),
    );
    return () => {
      live = false;
    };
  }, [path, count]);
  const nodes = new Set((ops ?? []).filter((o) => o.kind !== "limit").map((o) => o.node)).size > 1;
  return (
    <div className="oplist">
      <div className="op-head">
        Operations
        <Help label="About operations">
          What Virticle did on the node. A libvirt call is shown as the virsh command that does the same, run as root on the node, with the libvirt function beside it; a call without such a command shows the function and its arguments. Programs are shown as they ran. Reads are not listed. Passwords, keys and tokens are replaced by [redacted].
        </Help>
      </div>
      <ErrorBox error={error} />
      {ops && ops.length === 0 && <div className="muted">None</div>}
      {ops && ops.length > 0 && (
        <ol className="ops">
          {ops.map((o) => (
            <OpRow key={o.seq} o={o} nodes={nodes} />
          ))}
        </ol>
      )}
    </div>
  );
}
