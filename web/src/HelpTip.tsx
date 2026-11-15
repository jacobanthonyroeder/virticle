// A "?" button that shows a short explanation next to it (DESIGN.md: help goes into "?" popovers).
// It opens and closes at once, under the button and inside the window; a click outside, Escape or
// scrolling closes it.
import { CSSProperties, useEffect, useRef, useState } from "react";
import "./helptip.css";

const WIDTH = 340;
const GUTTER = 16;

export default function HelpTip({ text, label }: { text: string; label: string }) {
  const [at, setAt] = useState<CSSProperties | null>(null);
  const box = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!at) return;
    const away = (e: MouseEvent) => {
      if (box.current && !box.current.contains(e.target as Node)) setAt(null);
    };
    const key = (e: KeyboardEvent) => e.key === "Escape" && setAt(null);
    const close = () => setAt(null);
    document.addEventListener("mousedown", away);
    document.addEventListener("keydown", key);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("mousedown", away);
      document.removeEventListener("keydown", key);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [at]);
  const toggle = (el: HTMLButtonElement) => {
    if (at) return setAt(null);
    const r = el.getBoundingClientRect();
    const width = Math.min(WIDTH, window.innerWidth - 2 * GUTTER);
    const left = Math.max(GUTTER, Math.min(r.left, window.innerWidth - GUTTER - width));
    setAt({ left, top: r.bottom + 6, maxWidth: width });
  };
  return (
    <span className="helptip" ref={box}>
      <button type="button" className="helptipbtn" aria-label={label} aria-expanded={!!at} onClick={(e) => toggle(e.currentTarget)}>
        ?
      </button>
      {at && (
        <span className="helptippop" role="note" style={at}>
          {text}
        </span>
      )}
    </span>
  );
}
