import { describeWhen, NextRunLine } from "../ScheduleTime";
import ScheduleAssistant, { describeKeep, effectiveKeep, Freq, Keep, keepOf, Projection } from "../ScheduleAssistant";
import { browserZone, localToUtc, utcToLocal } from "../tz";
import DialogAdvanced from "./DialogAdvanced";
import { useEffect, useState } from "react";
import { ApiError, bytes, del, get, NodeStatus, Pool, post, put, Task, VMDetail } from "../api";
import { useRole } from "../auth";
import { useLive } from "../live";
import PoolSelect from "../PoolSelect";
import Select from "../Select";
import { ErrorBox, Field, Modal, NodePicker, Progress, useAction, useConfirm, usePrompt } from "../ui";
import { EncDot, NewBackupsDot } from "../BackupKey";
import { StatusLine } from "../Checks";
import SubNav, { SubPage } from "../SubNav";

interface Manifest {
  vm: string;
  uuid: string;
  created: string;
  sourceNode: string;
  live: boolean;
  tpm: boolean;
  /** The file with the state of the VM's emulated TPM 2.0; absent when the backup does not hold it. */
  tpmState?: string;
  disks: { target: string; file: string; virtualSize: number; bytes: number; sha256?: string; encrypted?: boolean }[];
  /** The backup key its encrypted files are encrypted to; absent for a backup without encrypted files. */
  recipient?: string;
  nvram?: string;
  notes: string[];
  /** "full" or "incremental"; backups from before incremental backups have none and are full. */
  kind?: "full" | "incremental";
  /** Why a backup is full when an incremental one might have been expected. */
  fullReason?: string;
}
export interface BackupEntry {
  node: number;
  nodeName: string;
  pool: string;
  dir: string;
  path: string;
  size: number;
  manifest?: Manifest;
  error?: string;
  vmExists: boolean;
  /** Where the backed-up VM is now, and whether it is shut off (for "Replace"). */
  vmNode?: number;
  vmNodeName?: string;
  vmShutOff?: boolean;
  /** How many backups a restore reads (1 for a full backup), and why it cannot be restored when a link is missing. */
  chain?: number;
  chainError?: string;
  /** Later backups that build on this one and go with it when it is deleted. */
  dependents?: number;
  /** A restore reads an encrypted file (this backup or one it builds on): it asks for the passphrase. */
  needsKey?: boolean;
}

const isIncremental = (b: BackupEntry) => b.manifest?.kind === "incremental";

const when = (iso: string) => new Date(iso).toLocaleString();
const url = (b: BackupEntry) => `/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}`;

const hasLaterOnes = (n?: number) => (n ?? 0) > 0;


/** What a restore of this backup does with the TPM. */
const tpmLine = (m: Manifest) =>
  m.tpmState ? " The TPM state is restored too." : m.tpm ? " Its TPM state is not included: the restored VM starts with an empty TPM." : "";

function usePools(node: number) {
  const [pools, setPools] = useState<Pool[]>([]);
  useEffect(() => {
    if (!node) return;
    get<Pool[]>(`/nodes/${node}/pools`).then(
      (p) => setPools(p.filter((x) => x.state === "running" && ["dir", "fs", "netfs"].includes(x.type))),
      () => setPools([]),
    );
  }, [node]);
  return pools;
}

function useNodes() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  return (nodes.data?.nodes ?? []).filter((n) => n.state === "online");
}

/** Where to put something: a node and a directory-backed pool on it. */
function Target(props: { node: number; pool: string; setNode: (n: number) => void; setPool: (p: string) => void }) {
  const nodes = useNodes();
  const pools = usePools(props.node);
  useEffect(() => {
    if (pools.length && !pools.some((p) => p.name === props.pool)) props.setPool(pools[0].name);
  }, [pools]);
  return (
    <>
      <Field label="Target node">
        <NodePicker label="Target node" nodes={nodes} value={props.node} onChange={props.setNode} />
      </Field>
      <Field label="Target pool" help="Only running directory-backed pools can hold backups.">
        <PoolSelect label="Target pool" pools={pools} value={props.pool} onChange={props.setPool} />
      </Field>
    </>
  );
}

function BackupNow(props: { d: VMDetail; onClose: () => void }) {
  const { d } = props;
  const act = useAction();
  const [node, setNode] = useState(d.node);
  const [pool, setPool] = useState("");
  const [keep, setKeep] = useState(7);
  // A backup into the schedule's target is pruned by the schedule's rules (the server decides; this only shows it).
  const sched = useLive(() => get<{ schedule: BackupSched | null }>(`/nodes/${d.node}/vms/${d.uuid}/backup-schedule`), `/nodes/${d.node}/vms/${d.uuid}/backup-schedule`, () => false);
  const sch = sched.data?.schedule;
  const bySchedule = !!sch && (sch.targetNode || d.node) === node && sch.pool === pool;
  const base = `/nodes/${d.node}/vms/${d.uuid}`;
  // Once the server accepted the backup the dialog closes; the Tasks dock follows it and shows what kind it was.
  const submit = async () => {
    if (await act.run(() => post(`${base}/backup`, { targetNode: node === d.node ? 0 : node, pool, keepLast: keep }))) props.onClose();
  };
  return (
    <Modal title={`Back up ${d.name}`} onClose={props.onClose}>
      <p className="muted">Incremental when possible, otherwise full. Snapshots are not included; the TPM 2.0 state is, so keep backups as safe as the VM.</p>
      <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
      {bySchedule ? (
        <p className="muted" title="This pool is the schedule's target: its rules decide which older backups go">
          Retention: schedule ({describeKeep(sch)}).
        </p>
      ) : (
        <Field label="Keep the newest" help="Older backups of this VM in this pool are deleted, except those a kept incremental backup builds on. 0 keeps all.">
          <input type="number" min={0} max={1000} value={keep} onChange={(e) => setKeep(Number(e.target.value))} />
        </Field>
      )}
      <ErrorBox error={act.error} />
      <div className="actions">
        <button className="primary" disabled={act.busy || !pool} onClick={submit}>
          Back up now
        </button>
      </div>
    </Modal>
  );
}

/** Restore a backup. The dialog closes once the server accepted it; only warnings of the restore keep it open to be read. */
function Restore(props: { b: BackupEntry; onClose: () => void }) {
  const { b } = props;
  const m = b.manifest!;
  const act = useAction();
  const confirm = useConfirm();
  const [replace, setReplace] = useState(false);
  const [name, setName] = useState(m.vm + "-restored");
  const [node, setNode] = useState(b.node);
  const [pool, setPool] = useState(b.pool);
  const [start, setStart] = useState(true);
  const [passphrase, setPassphrase] = useState("");
  const [warnings, setWarnings] = useState<string[]>([]);
  // Replacing needs the original VM on the target node and shut off.
  const whyNot = !b.vmExists
    ? `Replace is not possible: ${m.vm} no longer exists.`
    : b.vmNode !== node
      ? `Replace is not possible: ${m.vm} is on ${b.vmNodeName ?? "another node"}; choose that node under Advanced.`
      : !b.vmShutOff
        ? `Replace is not possible while ${m.vm} is running; shut it down first.`
        : "";
  const doReplace = replace && !whyNot;
  const submit = async () => {
    if (doReplace) {
      const ok = await confirm.ask({
        title: `Replace ${m.vm}`,
        message: `The existing VM ${m.vm} on the target node is deleted together with its disks, and replaced by the backup from ${when(m.created)}.`,
        confirm: "Replace VM",
      });
      if (!ok) return;
    }
    return act.run(async () => {
      const r = await post<{ task: number; warnings: string[] }>(`${url(b)}/restore`, { mode: doReplace ? "original" : "new", name, targetNode: node, pool, replace: doReplace, start, ...(b.needsKey ? { passphrase } : {}) });
      setPassphrase("");
      if (r.warnings?.length) setWarnings(r.warnings);
      else props.onClose();
    });
  };
  return (
    <Modal title={`Restore ${m.vm}`} onClose={props.onClose}>
      {confirm.dialog}
      {warnings.length > 0 ? (
        <>
          <p>Restore started.</p>
          {warnings.map((w) => (
            <StatusLine key={w} status="warning">
              {w}
            </StatusLine>
          ))}
          <footer>
            <button className="primary" autoFocus onClick={props.onClose}>
              Close
            </button>
          </footer>
        </>
      ) : (
        <>
          <p className="muted">
            {when(m.created)}.{tpmLine(m)} Snapshots are not included.
          </p>
          <Field label="Restore" hint={whyNot || undefined}>
            <Select
              label="Restore"
              value={doReplace ? "replace" : "new"}
              onChange={(v) => setReplace(v === "replace")}
              options={[
                { value: "new", label: "As a new VM" },
                { value: "replace", label: `Replace ${m.vm}`, disabled: !!whyNot },
              ]}
            />
          </Field>
          {!doReplace && (
            <Field label="New VM name">
              <input value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
          )}
          {b.needsKey && (
            <Field label="Backup key passphrase" hint="Encrypted backup.">
              <input type="password" value={passphrase} onChange={(e) => setPassphrase(e.target.value)} autoComplete="off" />
            </Field>
          )}
          <label className="check">
            <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} /> Start the VM afterwards
          </label>
          <DialogAdvanced>
            <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
          </DialogAdvanced>
          <ErrorBox error={act.error} />
          <div className="actions">
            <button className="primary" disabled={act.busy || !pool || (!!b.needsKey && !passphrase)} onClick={submit}>
              Restore
            </button>
          </div>
        </>
      )}
    </Modal>
  );
}

function BackupTable(props: { list: BackupEntry[]; global?: boolean; reload: () => void }) {
  const role = useRole();
  const [restoring, setRestoring] = useState<BackupEntry | undefined>();
  const [deleting, setDeleting] = useState<BackupEntry | undefined>();
  const act = useAction();
  const prompt = usePrompt();
  // An encrypted incremental backup is put together for the export, which needs the key: the passphrase is checked
  // first and buys a ticket for the one download.
  const exportKeyed = async (b: BackupEntry) => {
    const passphrase = await prompt.ask({ title: "Export encrypted backup", label: "Backup key passphrase", hint: "Put together and encrypted again.", confirm: "Export", type: "password" });
    if (!passphrase) return;
    await act.run(async () => {
      const r = await post<{ ticket: string }>(url(b) + "/export-key", { passphrase });
      const a = document.createElement("a");
      a.href = "/api" + url(b) + "/export?ticket=" + encodeURIComponent(r.ticket);
      a.download = "";
      document.body.appendChild(a);
      a.click();
      a.remove();
    });
  };
  const remove = async (b: BackupEntry) => {
    if (await act.run(() => del(url(b) + (hasLaterOnes(b.dependents) ? "?withLater=1" : "")))) {
      setDeleting(undefined);
      props.reload();
    }
  };
  return (
    <>
      <ErrorBox error={act.error} />
      {prompt.dialog}
      <table>
        <thead>
          <tr>
            {props.global && <th>VM</th>}
            <th>Taken</th>
            <th>Type</th>
            <th>Where</th>
            <th>Disks</th>
            <th className="num">Size</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {props.list.map((b) => (
            <tr key={b.node + b.path}>
              {props.global && (
                <td className="bkname">
                  <strong>{b.manifest?.vm ?? b.dir}</strong> {b.manifest && !b.vmExists && <span className="badge s-shutoff">VM gone</span>}
                </td>
              )}
              <td>
                {b.manifest ? when(b.manifest.created) : "?"}
                {b.manifest && <div className="muted">{b.manifest.live ? "live" : "shut off"}{b.manifest.nvram ? ", UEFI variables" : ""}</div>}
                {b.error && <StatusLine status="error">{b.error}</StatusLine>}
              </td>
              <td>
                {b.manifest && <strong>{isIncremental(b) ? "Incremental" : "Full"}</strong>}
                {b.manifest && isIncremental(b) && <div className="muted">changes only; needs {b.chain ? b.chain - 1 : "?"} earlier {b.chain === 2 ? "backup" : "backups"}</div>}
                {b.manifest && !isIncremental(b) && b.manifest.fullReason && <div className="muted">{b.manifest.fullReason}</div>}
                {b.manifest && (
                  <div title={b.manifest.recipient ? "Encrypted to the backup key " + b.manifest.recipient : undefined}>
                    <EncDot on={!!b.manifest.recipient} />
                  </div>
                )}
                {b.chainError && <StatusLine status="error">Cannot be restored: {b.chainError}</StatusLine>}
              </td>
              <td title={b.path}>
                {b.nodeName}: {b.pool}
              </td>
              <td>{b.manifest?.disks.map((x) => `${x.target} (${bytes(x.virtualSize)})`).join(", ")}</td>
              <td className="num">{bytes(b.size)}</td>
              <td className="num bkactions">
                <div className="actions" style={{ justifyContent: "flex-end" }}>
                  {b.manifest && (
                    <button disabled={!!b.chainError} onClick={() => setRestoring(b)}>
                      Restore
                    </button>
                  )}
                  {b.manifest && role !== "viewer" && (
                    <button disabled={act.busy} onClick={() => act.run(() => post(url(b) + "/verify"))} title={isIncremental(b) ? "Check every file and checksum, with the earlier backups it needs" : "Check every file and checksum"}>
                      Verify
                    </button>
                  )}
                  {b.manifest && role !== "viewer" && !b.chainError && isIncremental(b) && b.needsKey && (
                    <button onClick={() => exportKeyed(b)} title="Download as one tar file; needs the passphrase to put it together">
                      Export
                    </button>
                  )}
                  {b.manifest && role !== "viewer" && !b.chainError && !(isIncremental(b) && b.needsKey) && (
                    <a className="button" href={"/api" + url(b) + "/export"} download title={b.manifest.recipient ? "Download as one tar file; the files stay encrypted" : "Download as one tar file"}>
                      Export
                    </a>
                  )}
                  <button className="danger" onClick={() => setDeleting(b)}>
                    Delete
                  </button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {restoring && <Restore b={restoring} onClose={() => setRestoring(undefined)} />}
      {deleting && (
        <Modal title="Delete backup" onClose={() => setDeleting(undefined)}>
          <p>
            Delete the backup <strong>{deleting.dir}</strong> on {deleting.nodeName}? This cannot be undone.
          </p>
          {hasLaterOnes(deleting.dependents) && (
            <p>
              {deleting.dependents} later incremental {deleting.dependents === 1 ? "backup builds" : "backups build"} on this one and {deleting.dependents === 1 ? "is" : "are"} deleted with it.
            </p>
          )}
          <div className="actions">
            <button className="danger" disabled={act.busy} onClick={() => remove(deleting)}>
              Delete
            </button>
          </div>
        </Modal>
      )}
    </>
  );
}

interface BackupSched extends Keep {
  enabled: boolean;
  mode: "interval" | "daily";
  everyMinutes?: number;
  at?: string;
  targetNode: number;
  pool: string;
}

interface SchedForm {
  freq: Freq;
  keep: Keep;
  targetNode: number;
  pool: string;
}

/** The form's frequency of a stored schedule: a UTC daily time becomes browser time; minutes stay minutes unless whole hours. */
function freqFrom(s?: BackupSched | null): Freq {
  if (s?.mode === "interval") {
    const m = s.everyMinutes ?? 60;
    return m % 60 === 0 ? { mode: "interval", at: "03:00", every: m / 60, unit: "hours" } : { mode: "interval", at: "03:00", every: m, unit: "minutes" };
  }
  const at = s?.at ? (utcToLocal(s.at, Date.now(), browserZone())?.time ?? s.at) : "03:00";
  return { mode: "daily", at, every: 6, unit: "hours" };
}

/** The fields to send for a frequency: browser time becomes UTC. */
function freqBody(f: Freq) {
  if (f.mode === "interval") return { mode: "interval", everyMinutes: f.every * (f.unit === "hours" ? 60 : 1) };
  return { mode: "daily", at: localToUtc(f.at, Date.now(), browserZone())?.time ?? f.at };
}

/** The schedule form and summary; the schedule lives in the VM's metadata. */
function SchedulePanel({ d }: { d: VMDetail }) {
  const path = `/nodes/${d.node}/vms/${d.uuid}/backup-schedule`;
  const role = useRole();
  const cur = useLive(() => get<{ schedule: BackupSched | null; last?: number; lastError?: string; nextRun?: number; runner?: "this" | "disabled" | "other"; tick?: number }>(path), path, (e) => e.type === "node" || (e.type === "task" && e.task?.status !== "running"));
  const nodes = useNodes();
  const act = useAction();
  const confirm = useConfirm();
  const [form, setForm] = useState<SchedForm | undefined>();
  const sch = cur.data?.schedule;
  const nodeName = (id: number) => {
    const n = nodes.find((x) => x.id === (id || d.node));
    return n?.hostname || n?.addr || `node ${id || d.node}`;
  };
  const edit = () =>
    setForm({
      freq: freqFrom(sch),
      keep: keepOf(sch, { keepLast: 0, keepHourly: 0, keepDaily: 7, keepWeekly: 0, keepMonthly: 0 }),
      targetNode: sch?.targetNode || d.node,
      pool: sch?.pool ?? "",
    });
  const formBody = (f: SchedForm) => ({ ...freqBody(f.freq), targetNode: f.targetNode === d.node ? 0 : f.targetNode, pool: f.pool, ...effectiveKeep(f.freq, f.keep) });
  const save = async () => {
    if (!form) return;
    // A saved schedule is enabled.
    const body = formBody(form);
    if (await act.run(() => put(path, body))) {
      setForm(undefined);
      cur.reload();
    }
  };
  const remove = async () => {
    const ok = await confirm.ask({ title: "Remove backup schedule", message: `The backup schedule of ${d.name} is removed. Backups already made are kept.`, confirm: "Remove schedule" });
    if (ok && (await act.run(() => del(path)))) cur.reload();
  };
  const canEdit = role !== "viewer";
  return (
    <section className="card" id="backup-schedule">
      {confirm.dialog}
      <h3>Scheduled backups</h3>
      <ErrorBox error={cur.error} />
      {!form && (
        <>
          {sch ? (
            <p>
              {!sch.enabled && <><span className="badge">Paused</span>{" "}</>}
              {describeWhen(sch)} to {sch.pool} on {nodeName(sch.targetNode)}. Keep: {describeKeep(sch)}.
              {cur.data?.last ? ` Last backup ${when(new Date(cur.data.last * 1000).toISOString())}.` : " No backup there yet."}
            </p>
          ) : (
            <p className="muted">No schedule.</p>
          )}
          {sch && cur.data?.lastError && <StatusLine status="error">Cannot read the target: {cur.data.lastError}</StatusLine>}
          {sch && <NextRunLine nextRun={cur.data?.nextRun} runner={cur.data?.runner} enabled={sch.enabled} tick={cur.data?.tick} onElapsed={cur.reload} />}
          {canEdit && (cur.data || cur.error) && (
            <div className="actions">
              <button onClick={edit} title={sch ? undefined : "A failed run raises an alert"}>{sch ? "Edit schedule" : "Set up schedule"}</button>
              {sch && (
                <button className="danger" disabled={act.busy} onClick={remove}>
                  Remove
                </button>
              )}
            </div>
          )}
        </>
      )}
      {form && (
        <>
          <Target node={form.targetNode} pool={form.pool} setNode={(n) => setForm((f) => f && { ...f, targetNode: n })} setPool={(p) => setForm((f) => f && { ...f, pool: p })} />
          {form.pool && (
            <ScheduleAssistant
              freq={form.freq}
              onFreq={(freq) => setForm((f) => f && { ...f, freq })}
              keep={form.keep}
              onKeep={(keep) => setForm((f) => f && { ...f, keep })}
              units={["minutes", "hours"]}
              timeTitle="Browser time"
              preview={() => post<Projection>(`${path}/preview`, formBody(form))}
              deps={[form.targetNode, form.pool]}
              where="pool"
            />
          )}
          <ErrorBox error={act.error} />
          <div className="actions">
            <button className="primary" disabled={act.busy || !form.pool} onClick={save}>
              Save schedule
            </button>
            <button onClick={() => setForm(undefined)}>Cancel</button>
          </div>
        </>
      )}
    </section>
  );
}

const taskDone = (e: { type: string; task?: Task }) => e.type === "task" && e.task?.status !== "running";

export const BACKUP_PAGES: SubPage[] = [
  { id: "backups", label: "Backups" },
  { id: "schedule", label: "Schedule", cards: ["backup-schedule"] },
];

/** The Backups tab of a VM. */
export default function BackupsTab({ d }: { d: VMDetail; base: string }) {
  const list = useLive(() => get<BackupEntry[]>(`/backups?uuid=${d.uuid}`), "backups/" + d.uuid, taskDone);
  const [making, setMaking] = useState(false);
  return (
    <>
      <SubNav pages={BACKUP_PAGES} label="Backups">
        {(sec) =>
          sec === "schedule" ? (
            <SchedulePanel d={d} />
          ) : (
            <>
              <div className="head" style={{ marginTop: 0 }}>
                <div className="headtitle">
                  <h3>Backups</h3>
                  <NewBackupsDot />
                </div>
                <div className="actions">
                  <button className="primary" onClick={() => setMaking(true)}>
                    Back up now
                  </button>
                </div>
              </div>
              <ErrorBox error={list.error} />
              {!list.data && !list.error && <div className="empty">Loading</div>}
              {list.data && list.data.length === 0 && <div className="empty">No backups</div>}
              {list.data && list.data.length > 0 && <BackupTable list={list.data} reload={list.reload} />}
            </>
          )
        }
      </SubNav>
      {making && <BackupNow d={d} onClose={() => setMaking(false)} />}
    </>
  );
}

function uploadTar(node: number, pool: string, file: File, onProgress: (p: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/api/nodes/${node}/backups/import?pool=${encodeURIComponent(pool)}`);
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = `${xhr.status} ${xhr.statusText}`;
      try {
        msg = JSON.parse(xhr.responseText).error ?? msg;
      } catch {
        /* keep the status text */
      }
      reject(new ApiError(xhr.status, msg));
    };
    xhr.onerror = () => reject(new Error("upload failed: connection lost"));
    xhr.send(file);
  });
}

function ImportFile(props: { onClose: () => void; onDone: () => void }) {
  const nodes = useNodes();
  const [node, setNode] = useState(0);
  const [pool, setPool] = useState("");
  const [file, setFile] = useState<File | undefined>();
  const [progress, setProgress] = useState(0);
  const act = useAction();
  useEffect(() => {
    if (!node && nodes.length) setNode(nodes[0].id);
  }, [nodes]);
  const go = async () => {
    if (file && (await act.run(() => uploadTar(node, pool, file, setProgress)))) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title="Import a VM export" onClose={props.onClose}>
      <p className="muted">
        A <code>.vmexport.tar</code> file. It is checked while it uploads and stored as a backup in the chosen pool.
      </p>
      <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
      <Field label="File">
        <input type="file" accept=".tar" onChange={(e) => setFile(e.target.files?.[0])} />
      </Field>
      {act.busy && <Progress value={progress} />}
      <ErrorBox error={act.error} />
      <div className="actions">
        <button className="primary" disabled={act.busy || !file || !pool} onClick={go}>
          Upload and import
        </button>
      </div>
    </Modal>
  );
}

/** All backups on all nodes, including those of VMs that no longer exist. */
export function BackupsSection() {
  const list = useLive(() => get<BackupEntry[]>("/backups"), "backups", taskDone);
  const [importing, setImporting] = useState(false);
  return (
    <section>
      <div className="head">
        <div className="headtitle">
          <h2>Backups</h2>
          <NewBackupsDot />
        </div>
        <div className="actions">
          <button onClick={() => setImporting(true)}>Import from file</button>
        </div>
      </div>
      <ErrorBox error={list.error} />
      {!list.data && !list.error && <div className="empty">Loading</div>}
      {list.data && list.data.length === 0 && <div className="empty">No backups</div>}
      {list.data && list.data.length > 0 && <BackupTable list={list.data} global reload={list.reload} />}
      {importing && <ImportFile onClose={() => setImporting(false)} onDone={list.reload} />}
    </section>
  );
}
