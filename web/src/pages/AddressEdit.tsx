// "Edit <switch or card>" on the Network tab: which of the node's addresses on it carries the management connection
// (the address the controller uses for the node) and which receives migrations. Exactly one address of the node
// carries each; ticking a box here takes it from wherever it was.
//
// Management uses the same call as Settings > Connection > Edit connection (PUT /nodes/{id} with the new address):
// the controller tests the connection first and refuses an address it cannot reach, so it is never left with one.
// Migration uses PUT /nodes/{id}/migration-network ("" is the default: the management address).
import { FormEvent, useEffect, useState } from "react";
import { ApiError, get, NodeStatus, put } from "../api";
import { CheckItem, CheckList } from "../Checks";
import { ErrorBox, Modal, useAction } from "../ui";
import Help from "../Help";
import { MigNetInfo, MigNetResult, resultItems } from "./MigrationNetwork";
import { EditResponse, stepItems } from "./NodeConnection";

export interface EditAddr {
  address: string; // with prefix
  management?: boolean;
  migration?: boolean;
  vlan?: number;
  iface?: string;
}

const ipOf = (cidr: string) => cidr.split("/")[0];
const isIPv4 = (s: string) => /^\d{1,3}(\.\d{1,3}){3}$/.test(s);
const isIP = (s: string) => isIPv4(s) || s.includes(":");

/** The host and the port of a node address: "10.0.0.5", "10.0.0.5:2222", "[fe80::1]:22", "node1.lan". */
function splitHostPort(addr: string): { host: string; port: string } {
  const v6 = /^\[([^\]]+)\](?::(\d+))?$/.exec(addr);
  if (v6) return { host: v6[1], port: v6[2] ?? "" };
  const m = /^([^:]+):(\d+)$/.exec(addr);
  if (m) return { host: m[1], port: m[2] };
  return { host: addr, port: "" };
}

export function AddressEditDialog(props: { node: number; title: string; addrs: EditAddr[]; onClose: () => void; onSaved: (items: CheckItem[]) => void }) {
  const list = props.addrs.filter((a) => a.address && !/^fe80:/i.test(a.address));
  const [mig, setMig] = useState<MigNetInfo | undefined>();
  const [conn, setConn] = useState<NodeStatus | undefined>();
  const [loadErr, setLoadErr] = useState<string | undefined>();
  // The choices: the new management address ("" = unchanged) and the migration address (undefined = unchanged, "" = default).
  const [mgmt, setMgmt] = useState("");
  const [migTo, setMigTo] = useState<string | undefined>();
  const [steps, setSteps] = useState<CheckItem[] | undefined>();
  const act = useAction();
  useEffect(() => {
    let live = true;
    Promise.all([get<MigNetInfo>(`/nodes/${props.node}/migration-network`), get<{ nodes: NodeStatus[] }>("/nodes")]).then(
      ([m, ns]) => {
        if (!live) return;
        setMig(m);
        setConn(ns.nodes.find((x) => x.id === props.node));
      },
      (e) => live && setLoadErr(String(e.message ?? e)),
    );
    return () => {
      live = false;
    };
  }, [props.node]);

  const hp = conn ? splitHostPort(conn.addr) : undefined;
  const byName = !!hp && !isIP(hp.host);
  const curMgmt = mig?.management.address ?? "";
  const newMgmt = mgmt || curMgmt;
  const curMig = mig?.address ?? ""; // "" = follows management
  const effMig = migTo === undefined ? curMig : migTo;
  const migOf = (ip: string) => (effMig === "" ? ip === newMgmt : effMig === ip);
  const migOffered = (ip: string) => !!mig && (ip === newMgmt || mig.options.some((o) => o.address === ip));

  const mgmtWhy = (ip: string): string | undefined => {
    if (ip === newMgmt) return "The controller reaches the node at this address. To move it, tick Management on another address.";
    if (byName) return `The controller reaches this node by the name ${hp?.host}. Change that under Settings, Connection.`;
    if (!isIPv4(ip)) return "Use an IPv4 address here; an IPv6 management address is set under Settings, Connection.";
    return undefined;
  };
  const migWhy = (ip: string): string | undefined => {
    if (!migOffered(ip)) return "Not one of the node's migration choices.";
    if (effMig === "" && ip === newMgmt) return "Migration follows the management address by default. To move it, tick Migration on another address.";
    return undefined;
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!conn || !mig) return;
    const items: CheckItem[] = [];
    setSteps(undefined);
    const ok = await act.run(async () => {
      if (mgmt && mgmt !== curMgmt) {
        const addr = hp?.port ? `${mgmt}:${hp.port}` : mgmt;
        try {
          const r = await put<EditResponse>(`/nodes/${props.node}`, { addr, user: conn.user, jump: conn.jump ?? "", force: false });
          items.push({ status: "ok", detail: `Management address saved: ${mgmt}.` }, ...(r.test?.steps ? stepItems(r.test.steps) : []));
          items.push(...(r.warnings ?? []).map((t): CheckItem => ({ status: "warning", detail: t })), ...(r.notes ?? []).map((t): CheckItem => ({ status: "info", detail: t })));
        } catch (er) {
          if (er instanceof ApiError && er.data?.test?.steps) setSteps(stepItems(er.data.test.steps));
          throw er;
        }
      }
      if (migTo !== undefined && migTo !== curMig) {
        const r = await put<MigNetResult>(`/nodes/${props.node}/migration-network`, { address: migTo === newMgmt ? "" : migTo });
        items.push(...resultItems(r, "Migration network saved."));
      }
    });
    if (ok) props.onSaved(items);
  };

  const changed = (!!mgmt && mgmt !== curMgmt) || (migTo !== undefined && migTo !== curMig);
  return (
    <Modal title={`Edit ${props.title}`} onClose={props.onClose}>
      <form onSubmit={save}>
        {list.length === 0 ? (
          <p className="muted">No address on this switch</p>
        ) : !mig || !conn ? (
          loadErr ? null : <p className="muted">Reading the node's addresses</p>
        ) : (
          <table className="compact addredit">
            <thead>
              <tr>
                <th>Address</th>
                <th>
                  Management
                  <Help label="About Management">The address at which the controller reaches the node. The connection is tested before it is saved.</Help>
                </th>
                <th>
                  Migration
                  <Help label="About Migration">The address that receives migration data from the other nodes. By default it is the management address.</Help>
                </th>
              </tr>
            </thead>
            <tbody>
              {list.map((a) => {
                const ip = ipOf(a.address);
                const mw = mgmtWhy(ip);
                const gw = migWhy(ip);
                return (
                  <tr key={(a.iface ?? "") + a.address}>
                    <td className="mono">
                      {a.vlan ? <span className="muted">VLAN {a.vlan} </span> : null}
                      {ip}
                      <wbr />
                      {a.address.slice(ip.length)}
                    </td>
                    <td>
                      <label className="check" title={mw}>
                        <input type="checkbox" checked={ip === newMgmt} disabled={!!mw || act.busy} onChange={(e) => e.target.checked && setMgmt(ip === curMgmt ? "" : ip)} aria-label={`Management on ${ip}`} />
                      </label>
                    </td>
                    <td>
                      <label className="check" title={gw}>
                        <input
                          type="checkbox"
                          checked={migOf(ip)}
                          disabled={!!gw || act.busy}
                          onChange={(e) => setMigTo(e.target.checked ? (ip === newMgmt ? "" : ip) : "")}
                          aria-label={`Migration on ${ip}`}
                        />
                      </label>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {steps && <CheckList sort={false} label="Connection test" items={steps} />}
        <ErrorBox error={act.error ?? loadErr} />
        <footer>
          <button type="button" onClick={props.onClose}>
            Cancel
          </button>
          {list.length > 0 && (
            <button className="primary" disabled={act.busy || !changed}>
              Save
            </button>
          )}
        </footer>
      </form>
    </Modal>
  );
}
