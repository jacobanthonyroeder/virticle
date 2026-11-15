import { useEffect, useState } from "react";
import { ArrowLeft } from "lucide-react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { get, put, VMDetail } from "../api";
import { useLive } from "../live";
import { secOfCard } from "../SubNav";
import { useRole } from "../auth";
import { ErrorBox, StateBadge, useAction } from "../ui";
import Hardware, { HW_PAGES } from "./Hardware";
import SnapshotsTab, { SNAP_PAGES } from "./Snapshots";
import { PerformanceTab } from "./Performance";
import BackupsTab, { BACKUP_PAGES } from "./Backups";
import VMActions from "./VMActions";
import Overview from "./VMOverview";
import { TagEditor } from "./Tags";
import { ReplicaCard, ReplicationDialog, StaleCard } from "./Replication";

export default function VMDetailPage() {
  const { node: nodeParam, uuid = "" } = useParams();
  const node = Number(nodeParam);
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") ?? "overview";
  const vm = useLive(() => get<VMDetail>(`/nodes/${node}/vms/${uuid}`), `${node}/${uuid}`, (e) =>
    e.type === "node" ? true : e.type === "domain" ? e.uuid === uuid : e.task?.status !== "running",
  );
  const act = useAction();
  // libvirt sends no event when the guest agent comes up, so look again while it is not connected.
  const waitingForAgent = vm.data?.state === "running" && vm.data.agent !== "connected";
  useEffect(() => {
    if (!waitingForAgent) return;
    // Often for the first two minutes (a guest that is booting), rarely after that (a guest without the agent).
    let tries = 0;
    let h: ReturnType<typeof setTimeout>;
    const again = () => {
      h = setTimeout(() => {
        if (document.visibilityState !== "hidden") vm.reload();
        again();
      }, tries++ < 24 ? 5000 : 60000);
    };
    again();
    return () => clearTimeout(h);
  }, [waitingForAgent, vm.reload]);
  // After the tab changed, bring a card of it into view (instantly).
  const [jump, setJump] = useState<string | undefined>();
  const [repl, setRepl] = useState(false);
  useEffect(() => {
    if (!jump) return;
    document.getElementById(jump)?.scrollIntoView({ block: "start" });
    setJump(undefined);
  }, [jump, tab]);
  const base = `/nodes/${node}/vms/${uuid}`;
  const d = vm.data;

  if (!d) {
    return (
      <>
        <Link to="/datacenter/vms" className="backlink"><ArrowLeft size={14} aria-hidden="true" /> Virtual machines</Link>
        <ErrorBox error={vm.error} />
      </>
    );
  }
  // A card of a tab with sub pages: open that sub page, then bring the card into view.
  const subPages: Record<string, typeof HW_PAGES> = { hardware: HW_PAGES, snapshots: SNAP_PAGES, backups: BACKUP_PAGES };
  const goto = (t: string, card?: string) => {
    const sec = secOfCard(subPages[t] ?? [], card);
    setParams(t === "overview" ? {} : sec ? { tab: t, sec } : { tab: t });
    setJump(card);
  };
  const tabs = [
    ["overview", "Overview"],
    ["performance", "Performance"],
    ["hardware", "Hardware"],
    ["snapshots", "Snapshots"],
    ["backups", "Backups"],
  ];
  // "?tab=console" (the Console links elsewhere) is the Overview with the live console open in its card.
  const consoleOpen = tab === "console";
  const shown = consoleOpen ? "overview" : tab;
  const replica = d.state === "replica"; // the standby's inert copy: only the Overview, and no actions

  return (
    <>
      <Link to="/datacenter/vms" className="backlink"><ArrowLeft size={14} aria-hidden="true" /> Virtual machines</Link>
      <div className="head">
        <div className="titleline">
          <h1>
            {d.name} <StateBadge state={d.state} />
            {d.template && <span className="badge s-template">Template</span>}
          </h1>
          <TagEditor tags={d.tags ?? []} base={base} vmName={d.name} onChanged={vm.reload} />
        </div>
        <div className="actions">
          {!replica && <VMActions d={d} base={base} goto={goto} onChanged={vm.reload} run={act.run} busy={act.busy} openReplication={() => setRepl(true)} />}
        </div>
      </div>
      <ErrorBox error={act.error ?? vm.error} />
      {(d.storageDown ?? []).map((t) => (
        <p key={t} className="note" role="status">
          <strong>Storage not answering.</strong> {t}
        </p>
      ))}
      {replica && <ReplicaCard d={d} base={base} />}
      {d.state === "stale" && <StaleCard d={d} base={base} onChanged={vm.reload} />}
      <div className="tabs">
        {(replica ? tabs.slice(0, 1) : tabs).map(([id, label]) => (
          <button key={id} className={shown === id ? "on" : ""} onClick={() => setParams(id === "overview" ? {} : { tab: id })}>
            {label}
          </button>
        ))}
      </div>
      {(shown === "overview" || replica) && <Overview d={d} base={base} onChanged={vm.reload} openReplication={() => setRepl(true)} consoleOpen={consoleOpen && !replica} onConsoleClose={() => consoleOpen && setParams({})} />}
      {!replica && tab === "performance" && <PerformanceTab kind="vm" id={uuid} passthrough={d.passthroughDevices > 0} />}
      {!replica && tab === "hardware" && <Hardware d={d} base={base} onChanged={vm.reload} />}
      {!replica && tab === "snapshots" && <SnapshotsTab d={d} base={base} />}
      {!replica && tab === "backups" && <BackupsTab d={d} base={base} />}
      {!replica && tab === "xml" && <XMLEditor d={d} base={base} onSaved={vm.reload} />}
      {repl && <ReplicationDialog d={d} base={base} onChanged={vm.reload} onClose={() => setRepl(false)} />}
    </>
  );
}

function XMLEditor({ d, base, onSaved }: { d: VMDetail; base: string; onSaved: () => void }) {
  const [text, setText] = useState(d.xml);
  const [dirty, setDirty] = useState(false);
  const [saved, setSaved] = useState(false);
  const act = useAction();
  const viewer = useRole() === "viewer";
  // Follow outside changes until the user starts editing.
  useEffect(() => {
    if (!dirty) setText(d.xml);
  }, [d.xml, dirty]);
  const save = async () => {
    if (await act.run(() => put(`${base}/xml`, { xml: text }))) {
      setDirty(false);
      setSaved(true);
      onSaved();
    }
  };
  return (
    <div className="xml">
      <p className="muted">
        {d.state !== "shutoff" && "Saved changes apply the next time the VM is started from off."}
      </p>
      <textarea
        spellCheck={false}
        value={text}
        readOnly={viewer}
        aria-label="Domain XML"
        onChange={(e) => {
          setText(e.target.value);
          setDirty(true);
          setSaved(false);
        }}
      />
      <ErrorBox error={act.error} />
      <footer>
        {saved && <span className="muted">Saved.</span>}
        <button
          disabled={!dirty}
          onClick={() => {
            setText(d.xml);
            setDirty(false);
            act.clear();
          }}
        >
          Revert
        </button>
        <button className="primary" disabled={!dirty || act.busy} onClick={save}>
          Save
        </button>
      </footer>
    </div>
  );
}
