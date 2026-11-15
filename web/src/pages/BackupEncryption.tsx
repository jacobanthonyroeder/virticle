import { FormEvent, useEffect, useState } from "react";
import { get, post } from "../api";
import { BackupEncryption as Status, EncDot, useBackupEncryption } from "../BackupKey";
import { ErrorBox, Field, useAction, useConfirm } from "../ui";

/** Saves text or a response body as a file in the browser's downloads. */
function saveFile(name: string, data: Blob) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(data);
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 10000);
}

interface KeyAnswer {
  status: Status;
  keyFile?: string;
  keyFileName: string;
}

const RECIPE = `age -d -i virticle-backup-key.age -o vda.qcow2 vda.qcow2.age
qemu-img check vda.qcow2`;

/** Settings > Backup encryption: one key for VM backups and the controller's scheduled backups. */
export default function BackupEncryption() {
  const st = useBackupEncryption(true);
  const s = st.data;
  const confirm = useConfirm();
  const sw = useAction();
  const dl = useAction();
  const chg = useAction();
  const [pass1, setPass1] = useState("");
  const [pass2, setPass2] = useState("");
  // The recovery key file of a key made a moment ago, offered for download until the page is left.
  const [fresh, setFresh] = useState<{ name: string; text: string } | undefined>();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [twoStep, setTwoStep] = useState(false);
  const [cur, setCur] = useState("");
  const [new1, setNew1] = useState("");
  const [new2, setNew2] = useState("");
  const [changed, setChanged] = useState(false);
  useEffect(() => {
    get<{ enabled: boolean }>("/account/2fa").then((r) => setTwoStep(r.enabled), () => {});
  }, []);

  const turnOn = async (e: FormEvent) => {
    e.preventDefault();
    const ok = await sw.run(async () => {
      if (!s?.hasKey && pass1 !== pass2) throw new Error("The two passphrases differ.");
      const a = await post<KeyAnswer>("/backup-encryption/on", { passphrase: pass1, again: pass2 });
      if (a.keyFile) {
        setFresh({ name: a.keyFileName, text: a.keyFile });
        saveFile(a.keyFileName, new Blob([a.keyFile], { type: "application/octet-stream" }));
      }
    });
    if (ok) {
      setPass1("");
      setPass2("");
      st.reload();
    }
  };
  const turnOff = async () => {
    const yes = await confirm.ask({
      title: "Turn off encryption",
      message: "New backups are stored unencrypted. Encrypted backups still need the key and its passphrase.",
      confirm: "Turn off",
    });
    if (yes && (await sw.run(() => post("/backup-encryption/off")))) st.reload();
  };
  const download = async (e: FormEvent) => {
    e.preventDefault();
    await dl.run(async () => {
      const res = await fetch("/api/backup-encryption/key", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ password, code }) });
      if (!res.ok) {
        let msg = `${res.status} ${res.statusText}`;
        try {
          msg = (await res.json()).error ?? msg;
        } catch {
          /* not JSON */
        }
        throw new Error(msg);
      }
      saveFile("virticle-backup-key.age", await res.blob());
    });
    setPassword("");
    setCode("");
  };
  const change = async (e: FormEvent) => {
    e.preventDefault();
    setChanged(false);
    const ok = await chg.run(async () => {
      const a = await post<KeyAnswer>("/backup-encryption/passphrase", { current: cur, new: new1, again: new2 });
      if (a.keyFile) setFresh({ name: a.keyFileName, text: a.keyFile });
    });
    setCur("");
    if (ok) {
      setNew1("");
      setNew2("");
      setChanged(true);
    }
  };
  const need = (s?.vmBackups ?? 0) + (s?.controllerBackups ?? 0);

  return (
    <>
      {confirm.dialog}
      <div className="head">
        <div className="headtitle">
          <h1>Backup encryption</h1>
          {s && <EncDot on={s.on} />}
        </div>
      </div>
      <ErrorBox error={st.error} />
      <div className="cards">
        <section className="card" aria-label="Encryption">
          <h2>Encryption</h2>
          <p>VM backups and scheduled controller backups, with age. One key; its passphrase is not stored.</p>
          {s?.recipient && (
            <p className="muted wrap mono" title="Public key; backups encrypted to it need the recovery key file and its passphrase">
              {s.recipient}
            </p>
          )}
          {s && s.on && (
            <div className="actions">
              <button className="danger" disabled={sw.busy} onClick={turnOff}>
                Turn off encryption
              </button>
            </div>
          )}
          {s && !s.on && (
            <form onSubmit={turnOn} aria-label="Turn on encryption" autoComplete="off">
              <Field label={s.hasKey ? "Passphrase of the backup key" : "Passphrase"} hint={s.hasKey ? undefined : "At least 10 characters."}>
                <input type="password" value={pass1} onChange={(e) => setPass1(e.target.value)} autoComplete={s.hasKey ? "off" : "new-password"} required />
              </Field>
              {!s.hasKey && (
                <Field label="Passphrase again">
                  <input type="password" value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" required />
                </Field>
              )}
              <div className="actions">
                <button className="primary" disabled={sw.busy || !pass1}>
                  Turn on encryption
                </button>
              </div>
            </form>
          )}
          <ErrorBox error={sw.error} />
          {s?.hasKey && !s.on && need > 0 && (
            <p role="status">
              {need} encrypted {need === 1 ? "backup still needs" : "backups still need"} the key.
            </p>
          )}
          <p>
            <strong>Without the passphrase and the recovery key file, encrypted backups cannot be restored by anyone.</strong>
          </p>
        </section>

        {s?.hasKey && (
          <section className="card" aria-label="Recovery key file">
            <h2>Recovery key file</h2>
            <p>Keep it off this controller, with the passphrase. A restored controller has the key; a lost one does not.</p>
            {fresh && (
              <div className="actions">
                <button className="primary" onClick={() => saveFile(fresh.name, new Blob([fresh.text], { type: "application/octet-stream" }))}>
                  Download recovery key file
                </button>
              </div>
            )}
            {!fresh && (
              <form onSubmit={download} aria-label="Download recovery key file" autoComplete="off">
                <Field label="Your password">
                  <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
                </Field>
                {twoStep && (
                  <Field label="Two-step code" hint="Authenticator or recovery code.">
                    <input value={code} onChange={(e) => setCode(e.target.value)} required autoComplete="one-time-code" />
                  </Field>
                )}
                <ErrorBox error={dl.error} />
                <div className="actions">
                  <button disabled={dl.busy}>Download recovery key file</button>
                </div>
              </form>
            )}
            <p className="muted" title="With the age tool (age-encryption.org), on any machine; it asks for the passphrase">
              Decrypt by hand:
            </p>
            <pre className="mono wrap recipe">{RECIPE}</pre>
          </section>
        )}

        {s?.hasKey && (
          <section className="card" aria-label="Change passphrase">
            <h2>Change passphrase</h2>
            <p className="muted">Same key: older backups stay readable. The old recovery key file keeps the old passphrase.</p>
            <form onSubmit={change} autoComplete="off">
              <Field label="Current passphrase">
                <input type="password" value={cur} onChange={(e) => setCur(e.target.value)} required autoComplete="off" />
              </Field>
              <Field label="New passphrase" hint="At least 10 characters.">
                <input type="password" value={new1} onChange={(e) => setNew1(e.target.value)} required autoComplete="new-password" />
              </Field>
              <Field label="New passphrase again">
                <input type="password" value={new2} onChange={(e) => setNew2(e.target.value)} required autoComplete="new-password" />
              </Field>
              <ErrorBox error={chg.error} />
              <div className="actions">
                <button disabled={chg.busy}>Change passphrase</button>
              </div>
              {changed && (
                <p role="status" className="muted">
                  Changed. Download the recovery key file again.
                </p>
              )}
            </form>
          </section>
        )}
      </div>
    </>
  );
}
