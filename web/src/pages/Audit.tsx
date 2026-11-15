import { FormEvent, Fragment, useCallback, useEffect, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { AuditEntry, get } from "../api";
import OperationList from "../OperationList";
import Select from "../Select";
import { ErrorBox } from "../ui";

const when = (unix: number) => new Date(unix * 1000).toLocaleString();

export default function Audit() {
  const [user, setUser] = useState("");
  const [method, setMethod] = useState("");
  const [path, setPath] = useState("");
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [next, setNext] = useState(0);
  const [error, setError] = useState<string | undefined>();
  const [open, setOpen] = useState<Set<number>>(new Set());
  const toggle = (id: number) =>
    setOpen((o) => {
      const n = new Set(o);
      n.has(id) ? n.delete(id) : n.add(id);
      return n;
    });

  const load = useCallback(
    async (before: number) => {
      const q = new URLSearchParams({ limit: "100" });
      if (user) q.set("user", user);
      if (method) q.set("method", method);
      if (path) q.set("path", path);
      if (before) q.set("before", String(before));
      try {
        const r = await get<{ entries: AuditEntry[]; next: number }>("/audit?" + q);
        setEntries((old) => (before ? [...old, ...r.entries] : r.entries));
        setNext(r.next);
        setError(undefined);
      } catch (e: any) {
        setError(String(e.message ?? e));
      }
    },
    [user, method, path],
  );
  useEffect(() => {
    load(0);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
  const search = (e: FormEvent) => {
    e.preventDefault();
    load(0);
  };

  return (
    <>
      <div className="head">
        <h1>Audit log</h1>
      </div>
      <p className="muted">Changes, console openings and sign-ins. Request contents are never recorded.</p>
      <form className="row" onSubmit={search}>
        <input placeholder="User" value={user} onChange={(e) => setUser(e.target.value)} aria-label="Filter by user" />
        <Select auto label="Filter by method" value={method} onChange={setMethod} options={[{ value: "", label: "Any method" }, ...["POST", "PUT", "DELETE", "GET"].map((m) => ({ value: m, label: m }))]} />
        <input placeholder="Path contains" value={path} onChange={(e) => setPath(e.target.value)} aria-label="Filter by path" />
        <button className="primary">Search</button>
      </form>
      <ErrorBox error={error} />
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>User</th>
            <th>Address</th>
            <th>Request</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <Fragment key={e.id}>
              <tr>
                <td>{when(e.time)}</td>
                <td>
                  {e.user || "-"}
                  {e.token && <span className="muted"> (token {e.token})</span>}
                </td>
                <td className="mono">{e.ip}</td>
                <td className="mono wrap">
                  {e.method} {e.path}
                  {e.detail && <div className="muted">{e.detail}</div>}
                  {!!e.ops && (
                    <div>
                      <button type="button" className="row-open" aria-expanded={open.has(e.id)} onClick={() => toggle(e.id)}>
                        {open.has(e.id) ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
                        {e.ops === 1 ? "1 operation" : `${e.ops} operations`}
                      </button>
                    </div>
                  )}
                </td>
                <td>
                  <span className={"badge " + (e.status >= 400 ? "s-crashed" : "s-running")}>{e.status}</span>
                </td>
              </tr>
              {open.has(e.id) && (
                <tr className="ops-row">
                  <td colSpan={5}>
                    <OperationList audit={e.id} />
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
      {entries.length === 0 && !error && <div className="empty">No matching entries.</div>}
      {next > 0 && (
        <p>
          <button onClick={() => load(next)}>Load older entries</button>
        </p>
      )}
    </>
  );
}
