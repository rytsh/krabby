# Design

World: **Sunday Funnies**. The krabby pages site is a newspaper Sunday comics section: the halftone wordmark is the masthead, the hero is one splash panel, and every section beneath it is built from inked panels, caption boxes, speech balloons and clip-and-save coupons.

Source: `_docs/src/pages/index.astro`, `_docs/src/styles/global.css`. Approved comp: `.impeccable/mocks/comp-b-splash.png`.

## Tokens

| Token | Value | Role |
| --- | --- | --- |
| `--paper` | `#efe6cf` | Newsprint ground (with SVG grain noise) |
| `--paper-deep` | `#e4d8b8` | Scrollbar track, deep paper |
| `--white` | `#fbf8ef` | Panel and balloon fill |
| `--ink` | `#141414` | Outlines, text, code blocks |
| `--red` / `--red-deep` | `#e2352c` / `#b42019` | Marker underlines, primary button, arrows, hover |
| `--yellow` | `#f7e05a` | Caption boxes, chips, tape, selection |
| `--blue` / `--sky` | `#3c9ad0` / `#bfe1ee` | Splash field, panel tints |
| Panel tints | `#fde9e3`, `#d8ecd8` | Alternating panel grounds |
| `--line` | `3px solid var(--ink)` | Every panel, caption, chip and button border |
| `--dots-*` | radial-gradient Ben-Day | Halftone fills on captions, buttons, blue panels |

No box-shadows: depth comes from overlap, rotation and tape, never offset shadows.

## Type

- **Display:** Rakkas 400 (self-hosted via `@fontsource/rakkas`). Headlines, nav links, buttons, panel titles. Line-height about 0.92–1.05.
- **Hand lettering:** Shantell Sans 700 / 700 italic / 800 italic (`@fontsource/shantell-sans`). Body and every caption; captions are italic uppercase.
- **Mono:** JetBrains Mono 400/700. Only for code, commands, paths and tool names.

## Components

- **Caption box** (`.caption`, `.caption.tiny`): yellow with dots, 3px ink border, italic uppercase hand lettering. Used as narrator text and as panel headings. There are no eyebrow labels above headings.
- **Panel** (`.panel`): 3px ink border, white or tinted ground; grids of panels sit on 14px gutters.
- **Balloon** (`.bubble`, `.balloon`): white with ink border, fully rounded; the hero bubble has a skewed tail.
- **Chip** (`.chip`): yellow mono label for `path:line`, joined to its target by red hand-drawn SVG arrows.
- **Coupon** (`.coupon`): 3px dashed ink border on paper, yellow tape corners, scissors icon heading, ink code block with a Copy button.
- **Button** (`.btn`): Rakkas 22px, 3px border, min-height 46px; primary is red with dots. On hover it tilts −2°.
- **Marker underline** (`.ul`): red band under the last part of a heading; wraps per line.

## Imagery

Raster plates live in `_docs/public/plates/` (webp, transparent except `splash-bg`), all in one inked Ben-Day style: the crab detective with magnifier, the robot agent, the sources cabinet, the graph, four step icons and the splash backdrop. Each plate's generation prompt is in a `.webp.json` sidecar. New art must match: bold black outlines, red/yellow/blue halftone, slight misregistration.

## Layout and motion

- Page width `min(1520px, 100% - 32px)`. The splash is a 1500:650 absolutely-positioned panel; below 1100px it becomes a grid and the decorative plates drop out. Under 1380px the scrawl and coupon note are hidden.
- Each section starts with a 3px top rule and a heading row (display heading + hand-lettered dek).
- Motion is one moment: the magnifier settles in and the chips drop onto line 42. It is disabled under `prefers-reduced-motion`.
