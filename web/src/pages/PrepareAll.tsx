import { FormEvent, useEffect, useRef, useState } from "react";
import { get, NodeStatus, post, put } from "../api";
import { useEvents } from "../live";
import { Field, Modal } from "../ui";
import Select from "../Select";
import "./prepareall.css";

/**
 * A node that needs the Prepare step again: online, and the node command answers nothing (never prepared, prepared by an
 * older script, or an installed node command of another version). Offline nodes cannot say, so they are left out.
 */
export function prepareState(n: NodeStatus): "older" | "unprepared" | undefined {
  if (n.state !== "online" || n.detectedPrivilege !== "none") return undefined;
  return /older version|installed node command is version/.test(n.privilegeNote ?? "") ? "older" : "unprepared";
}

interface Job {
  rows: { id: string; title: string; status: string; detail?: string }[];
  done: boolean;
  ok: boolean;
  user: string;
}

type Phase = "waiting" | "running" | "ok" | "failed";
const WORD: Record<Phase, string> = { waiting: "Waiting", running: "Running", ok: "Prepared", failed: "Failed" };
const STATE: Record<Phase, string> = { waiting: "waiting", running: "connecting", ok: "ok", failed: "failed" };

/** One node of the run: starts its preparation when it is its turn and reports when that ended, whatever the result. */
function Step({ n, active, body, onDone }: { n: NodeStatus; active: boolean; body: object; onDone: () => void }) {
  const [phase, setPhase] = useState<Phase>("waiting");
  const [why, setWhy] = useState("");
  const [task, setTask] = useState<number>();
  const started = useRef(false);
  const ended = useRef(false);

  const finish = (ok: boolean, text: string) => {
    if (ended.current) return;
    ended.current = true;
    setPhase(ok ? "ok" : "failed");
    setWhy(text);
    onDone();
  };
  const load = (id: number) =>
    get<Job>(`/prepare/${id}`).then(
      async (j) => {
        if (!j.done || ended.current) return;
        if (!j.ok) return finish(false, (j.rows.find((r) => r.status === "error") ?? j.rows[j.rows.length - 1])?.detail ?? "Failed");
        try {
          if (j.user && j.user !== n.user) await put(`/nodes/${n.id}`, { user: j.user });
          finish(true, "");
        } catch (e: any) {
          finish(false, String(e.message ?? e));
        }
      },
      () => {},
    );
  useEffect(() => {
    if (!active || started.current) return;
    started.current = true;
    setPhase("running");
    post<{ task: number }>("/prepare?async=1", { addr: n.addr, jump: n.jump ?? "", checkOnly: false, allowRoot: n.user === "root", ...body }).then(
      (r) => {
        setTask(r.task);
        void load(r.task);
      },
      (e) => finish(false, String(e.message ?? e)),
    );
  });
  useEvents((e) => {
    if (e.type === "task" && task !== undefined && e.task?.id === task) void load(task);
  });
  return (
    <tr>
      <td>{n.hostname || n.addr}</td>
      <td>
        <span className={"badge s-" + STATE[phase]}>{WORD[phase]}</span>
        {why && <div className="muted pa-why">{why}</div>}
      </td>
    </tr>
  );
}

/**
 * Prepare every node that needs it with one administrator login (used for this run only, kept in memory and emptied at the end).
 * The nodes go one after another; a node that fails does not stop the rest.
 */
export default function PrepareAll({ nodes, onClose }: { nodes: NodeStatus[]; onClose: () => void }) {
  const [targets, setTargets] = useState<NodeStatus[]>();
  const [adminUser, setAdminUser] = useState("root");
  const [cred, setCred] = useState("password");
  const [secret, setSecret] = useState("");
  const [index, setIndex] = useState(0);

  const start = (e: FormEvent) => {
    e.preventDefault();
    setTargets(nodes.filter((n) => prepareState(n)));
  };
  const finished = !!targets && index >= targets.length;
  useEffect(() => {
    if (finished) setSecret("");
  }, [finished]);

  if (targets) {
    const body = { adminUser: adminUser.trim() || "root", password: cred === "password" ? secret : "", privateKey: cred === "key" ? secret : "" };
    return (
      <Modal title="Update Virticle on nodes" onClose={onClose}>
        <div className="table-scroll">
          <table>
            <tbody>
              {targets.map((n, i) => (
                <Step key={n.id} n={n} active={i === index} body={body} onDone={() => setIndex((x) => x + 1)} />
              ))}
            </tbody>
          </table>
        </div>
      </Modal>
    );
  }
  return (
    <Modal title="Update Virticle on nodes" onClose={onClose}>
      <form onSubmit={start} autoComplete="off">
        <div className="grid2">
          <Field label="Administrator user" hint="root, or a user that may use sudo. The same login must work on every node.">
            <input value={adminUser} onChange={(e) => setAdminUser(e.target.value)} required />
          </Field>
          <Field label="Credential">
            <Select
              label="Credential"
              value={cred}
              onChange={(v) => {
                setCred(v);
                setSecret("");
              }}
              options={[
                { value: "password", label: "Password" },
                { value: "key", label: "Private key" },
              ]}
            />
          </Field>
        </div>
        {cred === "password" ? (
          <Field label="Password" help="Used for this run only and not stored. Also used for sudo when the user is not root.">
            <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} required autoComplete="new-password" autoFocus />
          </Field>
        ) : (
          <Field label="Private key" hint="Without a passphrase. Used for this run only and not stored.">
            <textarea className="mono" rows={5} value={secret} onChange={(e) => setSecret(e.target.value)} required spellCheck={false} autoComplete="off" />
          </Field>
        )}
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary">
            Update
          </button>
        </footer>
      </form>
    </Modal>
  );
}
