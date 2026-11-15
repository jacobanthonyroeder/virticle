import { FormEvent, useCallback, useEffect, useState } from "react";
import { bytes, get, post, put } from "../api";
import ScheduleAssistant, { describeKeep, effectiveKeep, Freq, Keep, keepOf, Projection } from "../ScheduleAssistant";
import { ErrorBox, Field, StateBadge, useAction } from "../ui";
import { EncDot, useBackupEncryption } from "../BackupKey";

interface Rec {
  time: number;
  size?: number;
  encrypted?: boolean;
  via: "cli" | "download" | "daily";
  ok: boolean;
  error?: string;
}
interface Status extends Keep {
  /** On (the name is from when it could only run daily). */
  daily: boolean;
  mode: "daily" | "interval";
  /** HH:MM, server time. */
  at?: string;
  everyHours?: number;
  zone: string;
  nextRun?: number;
  dir: string;
  last?: Rec;
  lastDaily?: Rec;
  restoreCommand: string;
}

const when = (unix: number) => new Date(unix * 1000).toLocaleString();
const VIA = { cli: "command line", download: "download", daily: "scheduled backup" };

const freqOf = (st: Status): Freq =>
  st.mode === "interval" ? { mode: "interval", at: "03:30", every: st.everyHours ?? 24, unit: "hours" } : { mode: "daily", at: st.at ?? "03:30", every: 24, unit: "hours" };

const schedBody = (f: Freq, k: Keep) => ({ ...(f.mode === "interval" ? { mode: "interval", everyHours: f.every } : { mode: "daily", at: f.at }), ...effectiveKeep(f, k) });

const describeFreq = (st: Status) => (st.mode === "interval" ? `Every ${st.everyHours} ${st.everyHours === 1 ? "hour" : "hours"}` : `Daily at ${st.at} server time`);

/** Settings > Controller backup: the controller's own backup. Restore is only on the command line. */
export default function ControllerBackup() {
  const [st, setSt] = useState<Status | undefined>();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [twoStep, setTwoStep] = useState(false);
  const [pass1, setPass1] = useState("");
  const [pass2, setPass2] = useState("");
  const dl = useAction();
  const daily = useAction();
  const [form, setForm] = useState<{ freq: Freq; keep: Keep } | undefined>();
  const enc = useBackupEncryption();

  const load = useCallback(() => {
    get<Status>("/controller-backup").then(setSt, () => {});
  }, []);
  useEffect(load, [load]);
  useEffect(() => {
    get<{ enabled: boolean }>("/account/2fa").then((r) => setTwoStep(r.enabled), () => {});
  }, []);

  const download = async (e: FormEvent) => {
    e.preventDefault();
    if (pass1 !== pass2) {
      dl.run(async () => {
        throw new Error("The two passphrases differ.");
      });
      return;
    }
    const ok = await dl.run(async () => {
      const res = await fetch("/api/controller-backup/download", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password, code, passphrase: pass1 }),
      });
      if (!res.ok) {
        let msg = `${res.status} ${res.statusText}`;
        try {
          msg = (await res.json()).error ?? msg;
        } catch {
          /* not JSON */
        }
        throw new Error(msg);
      }
      const blob = await res.blob();
      const m = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") ?? "");
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = m ? m[1] : "virticle-backup.tar.gz";
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
    });
    setPassword(""); // gone from the page whatever the answer
    setCode("");
    if (ok) {
      setPass1("");
      setPass2("");
      load();
    }
  };

  const setDaily = async (on: boolean) => {
    if (await daily.run(async () => setSt(await put<Status>("/controller-backup", { daily: on })))) {
      if (on) setTimeout(load, 3000); // the first copy is written at once
      else setForm(undefined);
    }
  };
  const saveSchedule = async () => {
    if (form && (await daily.run(async () => setSt(await put<Status>("/controller-backup", schedBody(form.freq, form.keep)))))) setForm(undefined);
  };

  return (
    <>
      <div className="head">
        <div className="headtitle">
          <h1>Controller backup</h1>
          {enc.data && <EncDot on={enc.data.on} title={enc.data.on ? "Scheduled backups are encrypted to the backup key (Settings > Backup encryption)" : "Scheduled backups are not encrypted (Settings > Backup encryption)"} />}
        </div>
      </div>
      <div className="cards">
        <form className="card" onSubmit={download} aria-label="Download backup" autoComplete="off">
          <h2>Download backup</h2>
          <p>
            Users, node list, node SSH key, migration CA and web certificates. It does not hold VMs, storage or networks.
          </p>
          <p>
            <strong>The file contains private keys.</strong> Keep it as safe as a root password, or give it a passphrase below.
          </p>
          <Field label="Your password">
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
          </Field>
          {twoStep && (
            <Field label="Two-step code" hint="Authenticator or recovery code.">
              <input value={code} onChange={(e) => setCode(e.target.value)} required autoComplete="one-time-code" />
            </Field>
          )}
          <Field label="Passphrase (optional)" hint="Encrypts the file. A lost passphrase cannot be recovered.">
            <input type="password" value={pass1} onChange={(e) => setPass1(e.target.value)} autoComplete="new-password" />
          </Field>
          <Field label="Passphrase again">
            <input type="password" value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" disabled={!pass1} />
          </Field>
          <ErrorBox error={dl.error} />
          <button className="primary" disabled={dl.busy}>
            Download backup
          </button>
          <p className="muted" role="status">
            {st?.last ? `Last backup: ${when(st.last.time)}, by ${VIA[st.last.via]}${st.last.size ? `, ${bytes(st.last.size)}` : ""}${st.last.encrypted ? ", encrypted" : ""}.` : "No backup yet."}
          </p>
          <p className="muted">
            Restore, with the controller stopped: <code>{st?.restoreCommand ?? "sudo systemctl stop virticle && sudo virticle restore FILE"}</code>
          </p>
        </form>

        <div className="card" role="group" aria-label="Backups on this server">
          <h2>Backups on this server</h2>
          <label className="check">
            <input type="checkbox" checked={!!st?.daily} disabled={!st || daily.busy} onChange={(e) => setDaily(e.target.checked)} /> Keep backups on this server
          </label>
          <p>
            In <code>{st?.dir ?? "the backups folder"}</code>
            {st && (
              <>
                : {describeFreq(st)}, keep {describeKeep(st)}
              </>
            )}
            .
          </p>
          {enc.data && (
            <p title={enc.data.on ? "Encrypted to the backup key: restore needs the recovery key file and its passphrase (virticle restore --key FILE)" : "Only the controller's account can read them. Turn on encryption under Settings > Backup encryption"}>
              <EncDot on={enc.data.on} />
            </p>
          )}
          <p>
            Not a copy off this machine: use the download above or <code>virticle backup -o DIR --keep-daily 7</code> from cron.
          </p>
          <ErrorBox error={daily.error} />
          {st && !form && (
            <div className="actions">
              <button onClick={() => setForm({ freq: freqOf(st), keep: keepOf(st) })}>Edit schedule</button>
            </div>
          )}
          {st && form && (
            <>
              <ScheduleAssistant
                freq={form.freq}
                onFreq={(freq) => setForm((f) => f && { ...f, freq })}
                keep={form.keep}
                onKeep={(keep) => setForm((f) => f && { ...f, keep })}
                units={["hours"]}
                timeTitle={`Server time (${st.zone})`}
                preview={() => post<Projection>("/controller-backup/preview", schedBody(form.freq, form.keep))}
                deps={[]}
                where="file system"
              />
              <div className="actions">
                <button className="primary" disabled={daily.busy} onClick={saveSchedule}>
                  Save schedule
                </button>
                <button onClick={() => setForm(undefined)}>Cancel</button>
              </div>
            </>
          )}
          {st?.daily && st.lastDaily && (
            <p role="status">
              Last scheduled backup: {when(st.lastDaily.time)} <StateBadge state={st.lastDaily.ok ? "ok" : "failed"} />
              {!st.lastDaily.ok && st.lastDaily.error ? ` ${st.lastDaily.error}` : ""}
            </p>
          )}
          {st?.daily && st.nextRun && <p className="muted">Next: {when(st.nextRun)}</p>}
        </div>
      </div>
    </>
  );
}
