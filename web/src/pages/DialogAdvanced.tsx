import { ReactNode, useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import "./hw.css";

/**
 * The one collapsed "Advanced" part of a dialog or form. Closed every time the dialog opens, never remembered.
 * The contents stay mounted while it is closed, so they keep their values and take part in form validation: when a
 * required or invalid field inside is hit by a submit, the section opens and that field gets focus.
 * `open` and `onToggle` are optional, for a dialog that wants to open it itself. `title` replaces "Advanced" (every
 * collapsed part of a page or dialog uses this one look), `defaultOpen` sets the first state, `flat` drops the frame
 * where it already sits in a card.
 */
export default function DialogAdvanced({ children, open, onToggle, title = "Advanced", defaultOpen, flat }: { children: ReactNode; open?: boolean; onToggle?: (open: boolean) => void; title?: ReactNode; defaultOpen?: boolean; flat?: boolean }) {
  const [own, setOwn] = useState(!!defaultOpen);
  const isOpen = open ?? own;
  const pending = useRef<HTMLElement | null>(null);
  const set = (v: boolean) => {
    setOwn(v);
    onToggle?.(v);
  };
  useEffect(() => {
    const el = pending.current;
    if (el && isOpen) {
      pending.current = null;
      el.focus();
      (el as HTMLInputElement).reportValidity?.();
    }
  }, [isOpen]);
  const Chevron = isOpen ? ChevronDown : ChevronRight;
  return (
    <div
      className={flat ? "dlg-adv flat" : "dlg-adv"}
      onInvalidCapture={(e) => {
        if (isOpen || pending.current) return;
        pending.current = e.target as HTMLElement;
        set(true);
      }}
    >
      <button type="button" className="disc-head" aria-expanded={isOpen} onClick={() => set(!isOpen)}>
        <Chevron size={16} aria-hidden="true" />
        <span className="disc-title">{title}</span>
      </button>
      <div className="dlgadv" hidden={!isOpen}>{children}</div>
    </div>
  );
}
