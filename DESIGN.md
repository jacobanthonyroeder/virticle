# Virticle design

The rules for the web UI. The tokens live in `web/src/styles.css`; this file says how to use them. When code and
this file disagree, fix one of them in the same change.

## Principles

| Rule | Meaning |
| --- | --- |
| Professional and stable | The UI is a tool for administrators. Nothing moves, blinks or shifts. |
| Low resource use | No animation, no polling loops. Pages follow the event stream. Timers and charts pause in a hidden tab. |
| Everything fits | Nothing is cut off and the page never scrolls sideways, at 1280 px and at 390 px, in both themes. |
| Simple | No option the app can decide itself. Requirements are done automatically. Two buttons beat a mode checkbox. |
| Terse | Users are professionals. Text states the state or the fact. Help goes into "?" popovers. |

## Status: a dot and a word

Every state is shown the same way: an 8 px dot in the state's colour, then the state name in the normal text
colour. Examples: `● Running`, `● Offline`, `● Encrypted`, `● Not encrypted`.

- The dot carries the colour. Text is never coloured to show a state.
- No toggles, switches, pills, icons or coloured backgrounds for state. A setting that is on or off shows its state
  as a dot, and is changed with a button (or a checkbox in a form).
- Use the `badge s-<state>` classes; add a new `s-` class to the matching colour group instead of a new style.

| Colour | Token | Used for |
| --- | --- | --- |
| Green | `--ok` | running, online, ok, active, encrypted |
| Amber | `--warn` | paused, connecting, building, maintenance, warnings |
| Red | `--danger` | crashed, offline, failed, degraded, critical |
| Blue | `--accent` | replica |
| Orange | `--accent-2` | stale |
| Grey | `--muted` (dot outline only, or filled) | shut off, not encrypted, unknown |

## Colour

Dark is the default theme. Light is chosen with `data-theme="light"` on `<html>`. Use tokens only; no literal
colours in components.

| Token | Dark | Light | Use |
| --- | --- | --- | --- |
| `--bg` | `#36454f` | `#f8f8f8` | Page background |
| `--panel` | `#3f4f5a` | `#ffffff` | Cards, tables, inputs, buttons |
| `--panel-2` / `--chrome` | `#2c3942` | `#ffffff` | Top bar, sidebar, Tasks dock |
| `--line` | `#51626e` | `#d3d3d3` | Borders and dividers |
| `--text` | `#f4f6f7` | `#36454f` | Text |
| `--muted` | `#cdd3d7` | `#5f6b73` | Secondary text, table heads, labels |
| `--accent` | `#00bfff` | `#003366` | Primary button, focus ring, active tab, selection |
| `--accent-text` | `#0b2230` | `#ffffff` | Text on the accent colour |
| `--accent-2` | `#ffa500` | `#00bfff` | Second accent |
| `--link` | `#66d9ff` | `#003366` | Links |
| `--ok` | `#5be38d` | `#1a7f37` | Good states |
| `--warn` | `#ffb938` | `#9a5b00` | Warning states |
| `--danger` | `#ffada5` | `#b3261e` | Failed states, border of destructive buttons, error boxes |
| `--hover` | `#485a66` | `#eef2f5` | Hover background, notes |
| `--chart-1`, `--chart-2` | `#00bfff`, `#ffa500` | `#003366`, `#b45f00` | Chart series |
| `--screen` / `--term-bg` | `#1b252b` | `#1b252b` / `#ffffff` | Console preview, terminal |

## Type

| What | Font | Size and weight |
| --- | --- | --- |
| Body | Source Sans 3 (`--sans`) | 14 px, 400 |
| Page title `h1` | Source Sans 3 | 20 px, 700 |
| Card title `h2` | Source Sans 3 | 16 px, 700 |
| Sub heading `h3` | Source Sans 3 | 14 px, 700 |
| Table heads, hints | Source Sans 3 | 12.5 px, 600, `--muted` |
| Buttons | Source Sans 3 | 14 px, 500 (primary 600) |
| Code, IDs, keys, paths, terminal | JetBrains Mono (`--mono`) | 12.5 px, 400 |

- Fonts are bundled (latin subset, woff2). Nothing is fetched from outside.
- Numbers in columns are right aligned with tabular figures (`.num`).
- Sentence case everywhere. No all caps.

## Layout

- The app is one screen high. The top bar (48 px) and the resource tree (260 px) stay; the main area scrolls on its
  own; the Tasks dock sits under it.
- Main area: padding 22 px 28 px, at most 1280 px wide.
- A page is a header (`.head`: title, then actions on the right), then tabs, then content.
- Content is cards (`.cards`: grid, columns at least 420 px, gap 14 px) and tables. A card has 16 px 18 px padding,
  a 1 px `--line` border and an 8 px radius (`--radius`).
- Long tabs are split into sub pages listed in a left column (`SubNav`), not into one long scroll.
- Breakpoints: below 900 px the sidebar becomes a drawer; below 760 px cards and field rows stack.
- Tables keep every column on screen. Long values wrap (`.wrap`); they are never cut and never scroll sideways.

## Components

| Component | Rule |
| --- | --- |
| Button | Neutral by default. One `primary` per form or dialog. `danger` (red border, normal text) for destructive actions. |
| Destructive action | Always a confirm dialog that names the object. |
| Select, combo box, menu | The themed `Select`, `ComboInput` and `Menu` components, not native ones. |
| Field | Label above the input. A hint under it only for a rule (a length, a format) or a state of the value ("Applies after a restart"). An explanation goes behind a "?" by the label (`help`). |
| Icons | Lucide, 14 to 16 px, next to a label. An icon alone only for a well-known action, with an `aria-label`. |
| Errors | An `.error` box next to what failed, with the server's message. |
| Empty state | A dashed box with one short line. |
| Help | The `Help` component: a small grey "?" in a circle after the text it explains. The explanation shows in a box while the pointer is on it or it has the focus. No clickable words ("Why", "Details") for explanations. This is the only place for explanations. |
| Tasks | Long work runs as a task: progress in the Tasks dock, a stop "x" when it can be stopped. |
| Charts | `--chart-1` and `--chart-2`; values in a hover box at the cursor, with a legend. |
| Focus | 2 px `--accent` outline on every focusable element. |

## Motion

- No entry, exit or looping animation. No shimmer, blur, transforms or moving loading indicators.
- Allowed: a 0.12 s colour transition on hover and focus. `prefers-reduced-motion` removes that too.
- Menus, dialogs and drawers open instantly. Loading placeholders are static and keep the final size, so nothing
  shifts when data arrives.

## Wording

- State names, counts and values. One or two words where possible: "Shutoff", not "This VM is shut off, start it
  to view the console".
- No instructions for the obvious next step, and no explanation of what a standard control does.
- No ellipsis character and no "..." in any label, button, menu, placeholder or status text: "Move", not "Move…".
- Keep warnings about data loss or an irreversible effect, and error messages that say what failed.
- Button labels are verbs for the action: "Restore", "Turn off encryption", "Download recovery key file".
