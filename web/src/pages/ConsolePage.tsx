// /console/:node/:uuid — the console alone, full window: no top bar, no tree.
import { useEffect } from "react";
import { useParams, useSearchParams } from "react-router-dom";
import { get, VMDetail } from "../api";
import { useRole } from "../auth";
import { useLive } from "../live";
import { LiveConsole } from "./Console";
import "./console.css";

export default function ConsolePage() {
  const { node: nodeParam, uuid = "" } = useParams();
  const node = Number(nodeParam);
  const [params] = useSearchParams();
  const viewer = useRole() === "viewer";
  const vm = useLive(
    () => get<VMDetail>(`/nodes/${node}/vms/${uuid}`),
    `${node}/${uuid}`,
    (e) => (e.type === "node" ? true : e.type === "domain" ? e.uuid === uuid : false),
  );
  const d = vm.data;
  useEffect(() => {
    document.title = d?.name ?? "Console";
    return () => void (document.title = "Virticle");
  }, [d?.name]);

  const msg = (text: string) => <div className="empty" style={{ margin: 24 }}>{text}</div>;
  if (viewer) return msg("Only operators and administrators.");
  if (!d) return vm.error ? msg(vm.error) : null;
  if (d.state === "shutoff") return msg("Shutoff");
  if (!d.hasVNC && !d.hasSerial) return msg("No console");
  return <LiveConsole d={d} variant="page" serialFirst={params.get("kind") === "serial"} />;
}
