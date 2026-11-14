// A column of sub pages at the left of a long tab (VM Hardware, node Settings, ...). Only the selected sub page is
// rendered. The choice lives in the URL as `sec=<id>`, so reload, back and forward and links keep it.
// On a narrow area the column becomes one wrapping row above the page.
import { KeyboardEvent, ReactNode, useId } from "react";
import { useSearchParams } from "react-router-dom";
import "./subnav.css";

export interface SubPage {
  id: string;
  label: string;
  /** Element ids of the cards on this page: a link to one of them opens this page. */
  cards?: string[];
}

/** The id of the page that holds a card, for a link to that card. */
export function secOfCard(pages: SubPage[], card: string | undefined): string | undefined {
  return card ? pages.find((p) => p.cards?.includes(card))?.id : undefined;
}

/** The selected page: `sec` from the URL; none or an unknown one is the first page. */
export function useSubPage(pages: SubPage[]): [string, (id: string, replace?: boolean) => void] {
  const [params, setParams] = useSearchParams();
  const want = params.get("sec");
  const sel = pages.some((p) => p.id === want) ? want! : pages[0].id;
  const set = (id: string, replace?: boolean) =>
    setParams(
      (p) => {
        const n = new URLSearchParams(p);
        n.set("sec", id);
        return n;
      },
      { replace },
    );
  return [sel, set];
}

export default function SubNav({ pages, label, children }: { pages: SubPage[]; label: string; children: (sec: string) => ReactNode }) {
  const [sel, set] = useSubPage(pages);
  const uid = useId();
  const tabId = (id: string) => `${uid}-tab-${id}`;
  const move = (e: KeyboardEvent, i: number) => {
    const last = pages.length - 1;
    const next =
      e.key === "ArrowDown" || e.key === "ArrowRight" ? (i === last ? 0 : i + 1) : e.key === "ArrowUp" || e.key === "ArrowLeft" ? (i === 0 ? last : i - 1) : e.key === "Home" ? 0 : e.key === "End" ? last : -1;
    if (next < 0) return;
    e.preventDefault();
    set(pages[next].id, true);
    document.getElementById(tabId(pages[next].id))?.focus();
  };
  return (
    <div className="subwrap">
      <div className="sublayout">
        {/* "keep": the rule that hides a viewer's buttons must not hide these */}
        <div className="subnav keep" role="tablist" aria-orientation="vertical" aria-label={label}>
          {pages.map((p, i) => (
            <button
              key={p.id}
              id={tabId(p.id)}
              role="tab"
              aria-selected={sel === p.id}
              aria-controls={uid + "-panel"}
              tabIndex={sel === p.id ? 0 : -1}
              className={sel === p.id ? "on" : ""}
              onClick={() => set(p.id)}
              onKeyDown={(e) => move(e, i)}
            >
              {p.label}
            </button>
          ))}
        </div>
        <div className="subpage" role="tabpanel" id={uid + "-panel"} aria-labelledby={tabId(sel)}>
          {children(sel)}
        </div>
      </div>
    </div>
  );
}
