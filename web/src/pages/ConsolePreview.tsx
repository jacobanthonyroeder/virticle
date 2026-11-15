// The "Console" card on the VM overview: a still preview that refreshes only
// while it is on screen, and opens the console in a new tab or inside the card
// (the card then grows to the page width and the cards after it move down).
// No noVNC connection is held for the preview itself.
import { useCallback, useEffect, useRef, useState } from "react";
import { Minimize2, RefreshCw, Settings2 } from "lucide-react";
import { post, VMDetail } from "../api";
import { DropdownMenu } from "../Menu";
import { useRole } from "../auth";
import { ErrorBox, useAction } from "../ui";
import { CLICK_LABELS, ClickMode, useClickMode } from "../consolePrefs";
import { LiveConsole } from "./Console";
import "./console.css";

const REFRESH_MS = 10000;

/** True while the element intersects the viewport. */
function useOnScreen(ref: React.RefObject<Element | null>, on: boolean): boolean {
  const [seen, setSeen] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!on || !el || typeof IntersectionObserver === "undefined") {
      setSeen(false);
      return;
    }
    const io = new IntersectionObserver((es) => setSeen(es[es.length - 1].isIntersecting));
    io.observe(el);
    return () => io.disconnect();
  }, [ref, on]);
  return seen;
}

function useTabVisible(): boolean {
  const [v, setV] = useState(document.visibilityState !== "hidden");
  useEffect(() => {
    const f = () => setV(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", f);
    return () => document.removeEventListener("visibilitychange", f);
  }, []);
  return v;
}

interface Preview {
  url?: string;
  error?: string;
  at?: Date;
  busy: boolean;
  refresh: () => void;
}

/** Fetches the still image once when active, then every 10 s while active and (if repeat) the VM runs. */
function usePreview(node: number, uuid: string, active: boolean, repeat: boolean, boxRef: React.RefObject<HTMLElement | null>): Preview {
  const [url, setUrl] = useState<string>();
  const [error, setError] = useState<string>();
  const [at, setAt] = useState<Date>();
  const [busy, setBusy] = useState(false);
  const urlRef = useRef<string>();
  const ctl = useRef<AbortController>();
  const stopped = useRef(false); // the server said there is no screen (409): do not keep asking
  const last = useRef(0);

  const load = useCallback(
    async (force: boolean) => {
      if (!force && (stopped.current || Date.now() - last.current < 3000)) return;
      ctl.current?.abort();
      const c = (ctl.current = new AbortController());
      last.current = Date.now();
      setBusy(true);
      try {
        const px = (boxRef.current?.clientWidth ?? 640) * Math.min(window.devicePixelRatio || 1, 2);
        const w = Math.min(1280, Math.max(320, Math.ceil(px / 160) * 160));
        const res = await fetch(`/api/nodes/${node}/vms/${uuid}/screenshot?w=${w}&fmt=jpeg`, { signal: c.signal });
        if (!res.ok) {
          const msg = (await res.json().catch(() => null))?.error ?? `${res.status} ${res.statusText}`;
          if (res.status === 409) stopped.current = true;
          throw new Error(msg);
        }
        const next = URL.createObjectURL(await res.blob());
        if (urlRef.current) URL.revokeObjectURL(urlRef.current);
        urlRef.current = next;
        stopped.current = false;
        setUrl(next);
        setError(undefined);
        setAt(new Date());
      } catch (e) {
        if ((e as Error).name !== "AbortError") setError((e as Error).message);
      } finally {
        if (ctl.current === c) setBusy(false);
      }
    },
    [node, uuid, boxRef],
  );

  useEffect(() => {
    if (!active) return;
    stopped.current = false;
    load(false);
    if (!repeat) return () => ctl.current?.abort();
    const h = setInterval(() => load(false), REFRESH_MS);
    return () => {
      clearInterval(h);
      ctl.current?.abort();
    };
  }, [active, repeat, load]);

  useEffect(
    () => () => {
      if (urlRef.current) URL.revokeObjectURL(urlRef.current);
    },
    [],
  );
  const refresh = useCallback(() => {
    stopped.current = false;
    load(true);
  }, [load]);
  return { url, error, at, busy, refresh };
}

function ModeMenu() {
  const [mode, setMode] = useClickMode();
  return (
    <DropdownMenu
      chevron={false}
      className="ghost"
      ariaLabel="Console preview click behaviour"
      title="What a click on the preview does"
      label={<Settings2 size={16} aria-hidden />}
      items={(Object.keys(CLICK_LABELS) as ClickMode[]).map((m) => ({ label: CLICK_LABELS[m], checked: mode === m, run: () => setMode(m) }))}
    />
  );
}

/** open: the page was asked to show the live console (a Console link elsewhere); onClose: it was collapsed. */
export function ConsoleCard({ d, open: openNow, onClose, onChanged }: { d: VMDetail; open?: boolean; onClose?: () => void; onChanged?: () => void }) {
  const viewer = useRole() === "viewer";
  const [mode] = useClickMode();
  const [wanted, setInline] = useState(!!openNow);
  useEffect(() => {
    if (openNow) setInline(true);
  }, [openNow]);
  const add = useAction();
  const [addNote, setAddNote] = useState<string>();
  const boxRef = useRef<HTMLDivElement>(null);
  const wideRef = useRef<HTMLElement>(null);
  const alive = d.state === "running" || d.state === "paused";
  const kind: "vnc" | "serial" | null = d.hasVNC ? "vnc" : d.hasSerial ? "serial" : null;
  const canPreview = !viewer && alive && d.hasVNC;
  // The live console shows while the VM runs; a VM that is started later with the console asked for opens it then.
  const inline = wanted && alive && !!kind && !viewer;
  // The card grows in place to the width of the page and the height of the window; bring all of it into view.
  useEffect(() => {
    if (inline) wideRef.current?.scrollIntoView({ block: "nearest" });
  }, [inline]);
  const onScreen = useOnScreen(boxRef, canPreview && !inline);
  const tabVisible = useTabVisible();
  const p = usePreview(d.node, d.uuid, canPreview && !inline && onScreen && tabVisible, d.state === "running", boxRef);
  const open = () => {
    if (!kind) return;
    if (mode === "inline") setInline(true);
    else window.open(`/console/${d.node}/${d.uuid}${kind === "serial" ? "?kind=serial" : ""}`, "_blank", "noopener");
  };

  let note = "";
  if (viewer) note = "Only operators and administrators.";
  else if (!kind) note = d.serialPending === "add" ? "Serial console added; applies after power off and start" : "No console";
  else if (!alive) note = "Shutoff";
  else if (kind === "serial") note = "Serial console";

  if (inline) {
    const back = (
      <button
        onClick={() => {
          setInline(false);
          onClose?.();
        }}
        title="Disconnect and show the still preview" aria-label="Collapse">
        <Minimize2 size={15} aria-hidden />
        <span className="lbl">Collapse</span>
      </button>
    );
    return (
      <section className="card span cc cc-wide" aria-label="Console" ref={wideRef}>
        <div className="cc-head">
          <h3>Console</h3>
          <ModeMenu />
        </div>
        <LiveConsole d={d} variant="inline" extra={back} />
      </section>
    );
  }

  const clickable = !!kind && !viewer && alive;
  return (
    <section className="card cc cc-thumb" aria-label="Console">
      <div className="cc-head">
        <h3>Console</h3>
        {!viewer && <ModeMenu />}
      </div>
      <div className="cc-box" ref={boxRef}>
        {p.url && canPreview ? (
          <button className="cc-open" onClick={open} aria-label={`Open the console of ${d.name}`} title={mode === "inline" ? "Open the console here" : "Open the console in a new tab"}>
            <img src={p.url} alt={`Current screen of ${d.name}`} />
          </button>
        ) : clickable ? (
          <button className="cc-open" onClick={open} aria-label={`Open the console of ${d.name}`}>
            <span className="cc-note">{note || (p.error ? p.error : "Loading preview")}</span>
          </button>
        ) : (
          <div className="cc-note">{note}</div>
        )}
      </div>
      <div className="cc-foot">
        {canPreview ? (
          <>
            <button onClick={p.refresh} disabled={p.busy} aria-label="Refresh preview" title="Refresh preview">
              <RefreshCw size={14} aria-hidden /> Refresh
            </button>
            <span className="muted">
              {p.error ? `Preview failed: ${p.error}` : p.at ? `Updated ${p.at.toLocaleTimeString()}` : " "}
            </span>
          </>
        ) : !viewer && !kind && d.serialPending !== "add" ? (
          <>
            <button
              disabled={add.busy}
              onClick={() =>
                add.run(async () => {
                  const r = await post<{ note?: string; needsRestart: boolean }>(`/nodes/${d.node}/vms/${d.uuid}/serial-console`);
                  setAddNote(r.needsRestart ? r.note : undefined);
                  onChanged?.();
                })
              }
            >
              Add serial console
            </button>
            {addNote && <span className="muted">{addNote}</span>}
          </>
        ) : (
          <span className="muted">&nbsp;</span>
        )}
      </div>
      <ErrorBox error={add.error} />
    </section>
  );
}
