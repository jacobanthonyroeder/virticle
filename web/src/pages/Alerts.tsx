import { useEffect, useState } from "react";
import { Bell, X } from "lucide-react";
import { NavLink } from "react-router-dom";
import { Alert, CleanResult, MigRun, bytes, get, post } from "../api";
import { useRole } from "../auth";
import { useLive } from "../live";
import { StatusDot } from "../Checks";
import { ErrorBox, Modal, useAction, useConfirm } from "../ui";

/** Current alerts; refetched whenever the server announces a change. */
export function useAlerts(): Alert[] {
  const r = useLive(() => get<{ alerts: Alert[] }>("/alerts"), "alerts", (e) => e.type === "alert");
  return r.data?.alerts ?? [];
}

function ago(since: number): string {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - since));
  if (s < 90) return "just now";
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 129600) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

const OPEN_EVENT = "virticle-open-alerts";
/** Opens the alerts panel of the bell (from a toast, for example). */
export const openAlerts = () => window.dispatchEvent(new Event(OPEN_EVENT));

/** Bell in the navigation with a count; opens a panel listing the alerts. */
export function AlertBell() {
  const alerts = useAlerts();
  const [open, setOpen] = useState(false);
  const admin = useRole() === "admin";
  const critical = alerts.filter((a) => a.severity === "critical").length;
  const [result, setResult] = useState<CleanResult>();
  const confirm = useConfirm();
  const act = useAction();
  // Remove and Keep of the files a failed migration left (the journal, migjournal.go on the server).
  const leftover = async (a: Alert, what: "remove" | "keep") => {
    const id = a.id.slice("migleft:".length);
    const runs = await get<{ runs: MigRun[] }>("/migration-leftovers").catch(() => ({ runs: [] as MigRun[] }));
    const run = runs.runs.find((r) => String(r.id) === id);
    const items = (run?.files ?? []).map((f) => f.path);
    const ok = await confirm.ask(
      what === "remove"
        ? { title: "Remove files", message: `Files of the failed migration of ${run?.name ?? ""} on ${run?.dstName ?? ""}. Only files proven to be the ones the migration made are removed, and only while the VM is confirmed on ${run?.srcName ?? "the source"} alone.`, confirm: "Remove", items }
        : { title: "Keep files", message: `The files stay on ${run?.dstName ?? "the node"} as ordinary volumes. The controller no longer removes them.`, confirm: "Keep", items, danger: false },
    );
    if (!ok) return;
    await act.run(async () => {
      if (what === "remove") setResult(await post<CleanResult>(`/migration-leftovers/${id}/remove`));
      else await post(`/migration-leftovers/${id}/keep`);
    });
  };
  useEffect(() => {
    const on = () => setOpen(true);
    window.addEventListener(OPEN_EVENT, on);
    return () => window.removeEventListener(OPEN_EVENT, on);
  }, []);
  useEffect(() => {
    if (!open) return;
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [open]);
  return (
    <div className="alertbell">
      <button className="bell" onClick={() => setOpen(!open)} aria-label={`Alerts: ${alerts.length}`} aria-expanded={open} title="Alerts">
        <Bell size={16} aria-hidden="true" />
        <span className="name">Alerts</span>
        {alerts.length > 0 && <span className={"count " + (critical ? "critical" : "warning")}>{alerts.length}</span>}
      </button>
      {open && (
        <div className="alertpanel" role="dialog" aria-label="Alerts">
          <header>
            <strong>Alerts</strong>
            <button className="ghost" onClick={() => setOpen(false)} aria-label="Close alerts">
              <X size={16} aria-hidden="true" />
            </button>
          </header>
          {alerts.length === 0 ? (
            <div className="muted pad">Nothing needs attention.</div>
          ) : (
            <ul>
              {alerts.map((a) => (
                <li key={a.id} className={a.severity}>
                  <div className="atitle">
                    <StatusDot status={a.severity === "critical" ? "error" : "warning"} /> {a.link ? <NavLink to={a.link} onClick={() => setOpen(false)}>{a.title}</NavLink> : a.title}
                  </div>
                  <div className="amsg">{a.message}</div>
                  {a.kind === "migration-leftover" && admin && (
                    <div className="aactions">
                      <button className="danger" disabled={act.busy} onClick={() => leftover(a, "remove")}>
                        Remove
                      </button>
                      <button disabled={act.busy} onClick={() => leftover(a, "keep")}>
                        Keep
                      </button>
                    </div>
                  )}
                  <small>since {ago(a.since)}</small>
                </li>
              ))}
            </ul>
          )}
          {act.error && <ErrorBox error={act.error} />}
          <footer className="muted">
            Checked every minute.
            {admin && (
              <>
                {" "}
                <NavLink to="/alerts-settings" onClick={() => setOpen(false)}>Notifications</NavLink>
              </>
            )}
          </footer>
        </div>
      )}
      {confirm.dialog}
      {result && <CleanResultDialog result={result} onClose={() => setResult(undefined)} />}
    </div>
  );
}

/** What a Remove did: the files removed, and the files kept with the reason. */
function CleanResultDialog({ result, onClose }: { result: CleanResult; onClose: () => void }) {
  return (
    <Modal title={`Files of ${result.name} on ${result.node}`} onClose={onClose} wide>
      {result.note && result.removed.length === 0 && <p>{result.note}</p>}
      <h3>Removed</h3>
      {result.removed.length === 0 ? (
        <p className="muted">None</p>
      ) : (
        <ul className="confirm-items mono">
          {result.removed.map((f) => (
            <li key={f.path}>
              {f.path} ({bytes(f.size)})
            </li>
          ))}
        </ul>
      )}
      <h3>Kept</h3>
      {result.kept.length === 0 ? (
        <p className="muted">None</p>
      ) : (
        <ul className="confirm-items">
          {result.kept.map((f) => (
            <li key={f.path}>
              <span className="mono">{f.path}</span>: {f.reason}
            </li>
          ))}
        </ul>
      )}
      <footer>
        <button className="primary" onClick={onClose}>
          Close
        </button>
      </footer>
    </Modal>
  );
}

/** Small inline markers for the alerts that match, to put next to an object. */
export function AlertMarks({ alerts, match }: { alerts: Alert[]; match: (a: Alert) => boolean }) {
  const mine = alerts.filter(match);
  if (mine.length === 0) return null;
  return (
    <span className="marks">
      {mine.map((a) => (
        <span key={a.id} className={"mark " + a.severity} title={a.message}>
          {a.title}
        </span>
      ))}
    </span>
  );
}
