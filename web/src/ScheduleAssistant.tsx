// The schedule assistant: how often a backup runs, how many to keep per rule (keep last, hourly, daily, weekly,
// monthly), and the server's projection of what that keeps and how much space it takes. Used by VM > Backups >
// Schedule and by Settings > Controller backup. The projection comes from the server (the same code that prunes),
// fetched when a field changes; nothing polls.
import { useEffect, useRef, useState } from "react";
import { bytes } from "./api";
import { StatusLine } from "./Checks";
import Select from "./Select";
import { Field } from "./ui";
import "./scheduleassist.css";

export interface Keep {
  keepLast: number;
  keepHourly: number;
  keepDaily: number;
  keepWeekly: number;
  keepMonthly: number;
}

export interface Freq {
  mode: "daily" | "interval";
  /** HH:MM, in the time the form shows (browser time for VMs, server time for the controller). */
  at: string;
  every: number;
  unit: "minutes" | "hours";
}

export interface Projection {
  policy: { keepLast: number; keepHourly: number; keepDaily: number; keepWeekly: number; keepMonthly: number };
  ineffective: Record<string, string>;
  nextRuns?: number[];
  points: { time: number; reason: string; rules?: string[]; full: boolean }[];
  kept: number;
  oldest?: number;
  complete: boolean;
  unbounded?: boolean;
  zone: string;
  size?: { low: number; high: number; basis: "backups" | "disks" | "last"; full: number; incremental?: number; measured?: number; fulls: number; incrementals: number };
  free?: number;
  used: number;
  chains: boolean;
  note?: string;
}

const RULES = [
  { key: "keepLast", rule: "last", label: "Last", tip: "The newest N backups" },
  { key: "keepHourly", rule: "hourly", label: "Hourly", tip: "The newest backup of each of the last N hours" },
  { key: "keepDaily", rule: "daily", label: "Daily", tip: "The newest backup of each of the last N days" },
  { key: "keepWeekly", rule: "weekly", label: "Weekly", tip: "The newest backup of each of the last N weeks (Monday to Sunday)" },
  { key: "keepMonthly", rule: "monthly", label: "Monthly", tip: "The newest backup of each of the last N months" },
] as const;

const PRESETS: { name: string; keep: Keep }[] = [
  { name: "Short", keep: { keepLast: 0, keepHourly: 24, keepDaily: 7, keepWeekly: 0, keepMonthly: 0 } },
  { name: "Standard", keep: { keepLast: 0, keepHourly: 24, keepDaily: 7, keepWeekly: 4, keepMonthly: 6 } },
  { name: "Long", keep: { keepLast: 0, keepHourly: 0, keepDaily: 7, keepWeekly: 8, keepMonthly: 12 } },
];

/** Minutes between two runs. */
export const stepMinutes = (f: Freq) => (f.mode === "daily" ? 1440 : f.every * (f.unit === "hours" ? 60 : 1));

/** The rules that cannot have an effect at this frequency, with the reason (the server's rule, retention.Ineffective). */
export function ineffective(f: Freq): Record<string, string> {
  const m = stepMinutes(f);
  const out: Record<string, string> = {};
  if (m >= 1440) out.hourly = "Runs at most once a day: Daily keeps these";
  if (m >= 7 * 1440) out.daily = "Runs at most once a week: Weekly keeps these";
  if (m >= 28 * 1440) out.weekly = "Runs at most every four weeks: Monthly keeps these";
  return out;
}

/** The keep values that are stored: rules without effect are 0. */
export function effectiveKeep(f: Freq, k: Keep): Keep {
  const off = ineffective(f);
  return { ...k, keepHourly: off.hourly ? 0 : k.keepHourly, keepDaily: off.daily ? 0 : k.keepDaily, keepWeekly: off.weekly ? 0 : k.keepWeekly };
}

/** "last 3, 24 hourly, 7 daily". */
export function describeKeep(k: Partial<Keep>): string {
  const parts = RULES.filter((r) => (k[r.key] ?? 0) > 0).map((r) => (r.rule === "last" ? `last ${k[r.key]}` : `${k[r.key]} ${r.rule}`));
  return parts.length ? parts.join(", ") : "all";
}

export const keepOf = (s?: Partial<Keep> | null, fallback: Keep = { keepLast: 7, keepHourly: 0, keepDaily: 0, keepWeekly: 0, keepMonthly: 0 }): Keep =>
  s ? { keepLast: s.keepLast ?? 0, keepHourly: s.keepHourly ?? 0, keepDaily: s.keepDaily ?? 0, keepWeekly: s.keepWeekly ?? 0, keepMonthly: s.keepMonthly ?? 0 } : fallback;

const fmtDay = (unix: number) => new Date(unix * 1000).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
const fmtTime = (unix: number) => new Date(unix * 1000).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

interface Props {
  freq: Freq;
  onFreq: (f: Freq) => void;
  keep: Keep;
  onKeep: (k: Keep) => void;
  /** Units the interval may be given in. */
  units: Freq["unit"][];
  /** Tooltip of the time field (which clock it is in). */
  timeTitle?: string;
  /** Fetches the projection for the form values; changes of `deps` fetch again. */
  preview: () => Promise<Projection>;
  deps: unknown[];
  /** What the space is on: "pool" or "disk". */
  where: string;
}

export default function ScheduleAssistant(p: Props) {
  const { freq, keep } = p;
  const off = ineffective(freq);
  const [proj, setProj] = useState<Projection | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const seq = useRef(0);
  useEffect(() => {
    const n = ++seq.current;
    const h = setTimeout(() => {
      p.preview().then(
        (r) => {
          if (n === seq.current) {
            setProj(r);
            setErr(undefined);
          }
        },
        (e) => n === seq.current && setErr(String(e.message ?? e)),
      );
    }, 300);
    return () => clearTimeout(h);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [freq.mode, freq.at, freq.every, freq.unit, keep.keepLast, keep.keepHourly, keep.keepDaily, keep.keepWeekly, keep.keepMonthly, ...p.deps]);

  const setKeep = (key: keyof Keep, v: number) => p.onKeep({ ...keep, [key]: Number.isFinite(v) ? Math.max(0, Math.min(1000, Math.round(v))) : 0 });

  return (
    <div className="sa">
      <div className="grid2">
        <Field label="When">
          <Select
            label="When"
            value={freq.mode}
            onChange={(v) => p.onFreq({ ...freq, mode: v as Freq["mode"] })}
            options={[
              { value: "daily", label: "Daily at a fixed time" },
              { value: "interval", label: "At a fixed interval" },
            ]}
          />
        </Field>
        {freq.mode === "daily" ? (
          <label className="field" title={p.timeTitle}>
            <span>Time</span>
            <input type="time" value={freq.at} onChange={(e) => p.onFreq({ ...freq, at: e.target.value })} />
          </label>
        ) : (
          <div className="field" role="group" aria-label="Every">
            <span>Every</span>
            <div className="sa-every">
              <input type="number" aria-label="Every" min={1} max={freq.unit === "hours" ? 8784 : 527040} value={freq.every} onChange={(e) => p.onFreq({ ...freq, every: Math.max(1, Math.round(Number(e.target.value) || 1)) })} />
              {p.units.length > 1 ? (
                <Select
                  label="Unit"
                  value={freq.unit}
                  onChange={(v) => p.onFreq({ ...freq, unit: v as Freq["unit"] })}
                  options={p.units.map((u) => ({ value: u, label: u }))}
                />
              ) : (
                <span className="sa-unit">{freq.unit}</span>
              )}
            </div>
          </div>
        )}
      </div>

      <div className="field" role="group" aria-label="Keep">
        <span title="Kept by any rule: kept. Periods are counted in the controller's time zone.">Keep</span>
        <div className="sa-keep">
          {RULES.map((r) => {
            const why = off[r.rule];
            return (
              <label key={r.key} className="sa-n" title={why ?? r.tip}>
                <small>{r.label}</small>
                <input
                  type="number"
                  min={0}
                  max={1000}
                  aria-label={r.label}
                  disabled={!!why}
                  value={why ? 0 : keep[r.key]}
                  onChange={(e) => setKeep(r.key, Number(e.target.value))}
                />
              </label>
            );
          })}
        </div>
        <div className="sa-presets" role="group" aria-label="Presets">
          {PRESETS.map((pr) => (
            <button type="button" key={pr.name} title={describeKeep(pr.keep)} onClick={() => p.onKeep(pr.keep)}>
              {pr.name}
            </button>
          ))}
        </div>
      </div>

      <section className="sa-proj" aria-label="Projection">
        {err ? <StatusLine status="error">{err}</StatusLine> : proj ? <ProjectionView proj={proj} where={p.where} /> : <p className="muted">Projection: computing</p>}
      </section>
    </div>
  );
}

function ProjectionView({ proj, where }: { proj: Projection; where: string }) {
  const rows = RULES.filter((r) => proj.policy[r.key] > 0).map((r) => {
    const pts = proj.points.filter((x) => x.rules?.includes(r.rule));
    return { label: r.label, tip: r.tip, n: pts.length, of: proj.policy[r.key], newest: pts[0]?.time, oldest: pts[pts.length - 1]?.time, hourly: r.rule === "hourly" || r.rule === "last" };
  });
  const needed = proj.points.filter((x) => x.reason === "needed");
  const sz = proj.size;
  const room = proj.free !== undefined ? proj.free + proj.used : undefined;
  const tight = sz && room !== undefined && sz.high > room;
  const fmt = (n: number) => bytes(n);
  return (
    <>
      {proj.nextRuns && proj.nextRuns.length > 0 && (
        <p className="sa-line">
          <strong>Next:</strong> {proj.nextRuns.map(fmtTime).join(", ")}
        </p>
      )}
      <p className="sa-line" role="status" title={`Steady state: the schedule run on paper up to now. Periods in ${proj.zone}.`}>
        <strong>Kept:</strong>{" "}
        {proj.unbounded
          ? "every backup"
          : `${proj.kept} ${proj.kept === 1 ? "backup" : "backups"}${proj.oldest ? `, back to ${fmtDay(proj.oldest)}` : ""}`}
        {!proj.complete && <span className="muted"> (runs too frequent to project fully)</span>}
      </p>
      {!proj.unbounded && rows.length > 0 && (
        <table className="sa-table">
          <thead>
            <tr>
              <th>Rule</th>
              <th className="num">Kept</th>
              <th>Newest</th>
              <th>Oldest</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.label}>
                <td title={r.tip}>{r.label}</td>
                <td className="num" title={r.n < r.of ? "Fewer: the same backups count for several rules, or not enough periods yet" : undefined}>
                  {r.n}
                </td>
                <td>{r.newest ? (r.hourly ? fmtTime(r.newest) : fmtDay(r.newest)) : ""}</td>
                <td>{r.oldest ? (r.hourly ? fmtTime(r.oldest) : fmtDay(r.oldest)) : ""}</td>
              </tr>
            ))}
            {needed.length > 0 && (
              <tr>
                <td title="Older backups that kept incremental backups build on; chains are kept whole">Chains</td>
                <td className="num">{needed.length}</td>
                <td>{fmtDay(needed[0].time)}</td>
                <td>{fmtDay(needed[needed.length - 1].time)}</td>
              </tr>
            )}
          </tbody>
        </table>
      )}
      {sz && !proj.unbounded && (
        <p className="sa-line" title={sizeBasis(proj)}>
          <strong>Space:</strong> about {sz.high - sz.low < sz.high / 10 ? fmt(Math.round((sz.low + sz.high) / 2)) : `${fmt(sz.low)} to ${fmt(sz.high)}`}
          {sz.fulls + sz.incrementals > 0 && proj.chains && (
            <span className="muted">
              {" "}
              ({sz.fulls} full, {sz.incrementals} incremental)
            </span>
          )}
          {proj.free !== undefined && (
            <>
              {"; "}
              {fmt(proj.free)} free in the {where}
            </>
          )}
        </p>
      )}
      {sz?.basis === "disks" && <p className="muted sa-line">No backup yet: estimate from disk usage.</p>}
      {proj.note && <p className="muted sa-line">{proj.note}.</p>}
      {tight && <StatusLine status="warning">The estimate exceeds the free space of the {where}.</StatusLine>}
    </>
  );
}

function sizeBasis(p: Projection): string {
  const s = p.size!;
  if (s.basis === "last") return `Size of the last backup (${bytes(s.full)}) times the number kept.`;
  const full = s.basis === "disks" ? `disk usage ${bytes(s.full)} per full backup` : `last full backup ${bytes(s.full)}`;
  const inc = !p.chains
    ? "every backup full"
    : s.incremental
      ? `incremental average ${bytes(s.incremental)} (of ${s.measured})`
      : "incremental size unknown: 2 to 20% of a full backup each";
  return `Estimate from ${full}, ${inc}; a full backup after every 6 incremental ones and for each weekly and monthly backup. In use there now: ${bytes(p.used)}.`;
}
