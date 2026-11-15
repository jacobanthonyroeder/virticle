// Help: a small "?" in a circle. The text shows in a box while the pointer is on it or it has the keyboard focus
// (a tap or click shows it too). This is the one way explanations are offered; see DESIGN.md.
// Put it inside the element that holds the text it follows (a span, not a flex row next to the text), so that it
// sits on the text's line.
import { ReactNode, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CircleHelp } from "lucide-react";
import "./help.css";

export default function Help({ children, label = "Help" }: { children: ReactNode; label?: string }) {
  const [over, setOver] = useState(false);
  const [focus, setFocus] = useState(false);
  const open = over || focus;
  const btn = useRef<HTMLButtonElement>(null);
  const pop = useRef<HTMLSpanElement>(null);
  const id = useId();
  // Under the mark, kept inside the window; above it when there is no room below.
  useLayoutEffect(() => {
    const b = btn.current?.getBoundingClientRect();
    const p = pop.current;
    if (!open || !b || !p) return;
    const w = p.offsetWidth;
    const h = p.offsetHeight;
    const left = Math.max(8, Math.min(b.left, window.innerWidth - w - 8));
    const top = b.bottom + 6 + h > window.innerHeight - 8 && b.top - 6 - h > 8 ? b.top - 6 - h : b.bottom + 6;
    p.style.left = left + "px";
    p.style.top = top + "px";
  }, [open]);
  return (
    <span className="help" onMouseEnter={() => setOver(true)} onMouseLeave={() => setOver(false)}>
      <button
        type="button"
        ref={btn}
        className="helpbtn"
        aria-label={label}
        aria-describedby={open ? id : undefined}
        onFocus={() => setFocus(true)}
        onBlur={() => setFocus(false)}
        onClick={(e) => {
          e.preventDefault(); // inside a label: do not pass the click on to the field
          btn.current?.focus();
        }}
        onKeyDown={(e) => e.key === "Escape" && btn.current?.blur()}
      >
        <CircleHelp size={14} aria-hidden />
      </button>
      {open && createPortal(<span className="helppop" role="tooltip" id={id} ref={pop}>{children}</span>, document.body)}
    </span>
  );
}
