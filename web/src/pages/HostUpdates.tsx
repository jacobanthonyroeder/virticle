import { useEffect, useState } from "react";
import { get, HostUpdates as Updates } from "../api";
import { StatusLine } from "../Checks";
import { useRole } from "../auth";
import RollingUpdate from "./RollingUpdate";

/** Pending package updates of a node (read-only; nothing is installed from here). */
export default function HostUpdates({ node, online }: { node: number; online: boolean }) {
  const [u, setU] = useState<Updates | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  const [updating, setUpdating] = useState(false);
  const admin = useRole() === "admin";
  const load = (refresh: boolean) => {
    setBusy(true);
    get<Updates>(`/nodes/${node}/updates${refresh ? "?refresh=1" : ""}`)
      .then((d) => {
        setU(d);
        setErr(d.error);
      })
      .catch((e) => setErr(String(e.message ?? e)))
      .finally(() => setBusy(false));
  };
  useEffect(() => {
    setU(undefined);
    if (online) load(false);
  }, [node, online]);
  if (!online) return null;
  if (err) return <StatusLine status="error">Updates: {err}</StatusLine>;
  if (!u) return <span className="muted">{busy ? "Checking updates" : ""}</span>;
  if (u.family === "unsupported") return <span className="muted" title={u.note}>Updates not checked ({u.distro || "unknown system"})</span>;
  const n = u.updates.length;
  const hasSource = u.updates.some((p) => p.source);
  return (
    <div className="hostupdates">
      <div className="upd-head">
        <span className="upd-count">{n === 0 ? "Up to date" : `${n} update${n === 1 ? "" : "s"} available`}</span>
        {u.rebootRequired && (
          <span className="badge s-paused" title={u.rebootReason}>
            reboot required
          </span>
        )}
        <span className="upd-spacer" />
        <button disabled={busy} onClick={() => load(true)}>
          Check again
        </button>
        {admin && (
          <button className="primary" onClick={() => setUpdating(true)} title="Put this node in maintenance, move its virtual machines away, install the updates, restart if needed, then move them back">
            Update this node
          </button>
        )}
      </div>
      {updating && <RollingUpdate nodes={[node]} title="Update this node" onClose={() => { setUpdating(false); load(true); }} />}
      {u.rebootRequired && u.rebootReason && <StatusLine status="warning">{u.rebootReason}</StatusLine>}
      {n > 0 && (
        <table className="upd-table">
          <thead>
            <tr>
              <th>Package</th>
              <th>Installed</th>
              <th>Available</th>
              {hasSource && <th className="upd-src">Source</th>}
            </tr>
          </thead>
          <tbody>
            {u.updates.map((p) => (
              <tr key={p.name}>
                <td className="mono">{p.name}</td>
                <td className="mono muted">{p.from || ""}</td>
                <td className="mono">{p.version}</td>
                {hasSource && <td className="upd-src muted">{p.source || ""}</td>}
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="upd-note muted">
        {u.note} {u.metadataAt ? `Package lists last refreshed ${new Date(u.metadataAt * 1000).toLocaleString()}. ` : ""}Checked {new Date(u.checkedAt * 1000).toLocaleTimeString()}.
      </p>
    </div>
  );
}
