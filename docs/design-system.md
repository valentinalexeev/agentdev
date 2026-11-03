# Design system

The visual identity of agentdev: colours, type, shapes, the mark and the components shared by the management UI
(`images/ui`) and the product website. The files are in [`design-system/`](../docs/design-system/); this page is the
brand book and the reference for using them.

The identity says what the product does: **agents propose, people merge.**

## Files

| File | What | Edit? |
|---|---|---|
| `design-system/tokens.json` | every colour (light and dark), type style, spacing step, radius and shadow, each with a usage note | yes: the single source |
| `design-system/tokens.css` | the tokens as CSS custom properties and type classes | no: run `node scripts/design-tokens.mjs` |
| `design-system/fonts.css` | Google Fonts import for the three faces | replace it to self-host the fonts |
| `design-system/components.css` | the components below as CSS classes (`ad-` prefix) | yes |
| `design-system/agentdev-mark.svg`, `agentdev-mark-dark.svg`, `favicon.svg` | the mark for light grounds, dark grounds, and the browser tab | yes, with care |

Load order on a page: `fonts.css`, `tokens.css`, `components.css`. The theme follows the system setting;
`data-theme="light"` or `data-theme="dark"` on `<html>` forces one.

`node scripts/design-tokens.mjs --check` fails if `tokens.css` does not match `tokens.json`.

## Two actors, two colours

Every screen shows work passing between agents and people. The palette shows who acts before the words are read.

- **`lamp` (amber) is the agent.** An indicator lamp on an equipment panel: lit while an agent works, dark when it
  has scaled to zero. Use it for agent activity, agent lanes in diagrams and the agent half of the mark. It is a fill
  only; amber text uses `lamp-ink`.
- **`gate` (petrol) is the person.** The review gate every change passes. Use it for the primary button, Approve,
  Merge, the review phase, links and the focus ring. A gate-coloured element means "a person decides here".
- **Everything else is the panel:** `ground`, `panel`, `sunken`, `line`, `line-strong`, `ink`, `ink-muted`. Cool,
  slightly green greys, like painted equipment. No pure white grounds, no gradients.
- **States are separate from the two actors:** `ok`, `danger`, `paused`, each with a `-soft` tint. Every state also
  carries a word or a shape, so no state depends on hue alone.

Never use `lamp` for a human action or `gate` for an agent action. A screen with neither uses no brand colour.

**Agent identity.** Each agent gets one of six lane hues (`lane-1` to `lane-6`), picked by a hash of its name, so an
agent has the same colour on every screen. The hash: `x = (x * 31 + charCode) >>> 0` over the name, lane `x % 6 + 1`.

**Contrast.** Every text token reaches 4.5:1 on the grounds its usage note names, in both themes. `line-strong` and
the focus ring reach 3:1. A lit lamp has a `lamp-ink` edge, because amber on the ground is below 3:1.

## Voice

The product's documents set the voice: plain, exact, short sentences. The manuals follow ASD-STE100
([Writing the manuals](writing.md)); UI and website copy follow the same spirit.

- Write what happens: "The agent stops after 15 minutes with no work." Not "Smart idle detection saves you money!"
- Name things by what a person sees: issue, pull request, review, agent, limit. Use cluster words (pod, StatefulSet,
  PVC) only on admin screens.
- Sentence case in headings and buttons. Verb first on buttons: "Approve", "Create agent", "Resume".
- An agent always says it is an agent. On the forge, agent text starts with the marker the process requires; in the
  UI the marker is an agent tag, never a mascot or a robot picture.
- Numbers carry units and limits: "64% of the 5-hour window, resets 14:00", "2 / 3 running".
- No exclamation marks, no emoji in the UI, no em dashes.
- The website tells the truth about limits. A feature that is not released is "planned", with the issue link.

## Typography

Three faces, each with one job (`--font-display`, `--font-sans`, `--font-mono`).

- **Big Shoulders Display** is panel signage. Lowercase on the website (`display-1`, `display-2`), sentence case for
  console page titles (`display-3`). Never for running text or buttons.
- **Archivo** is everything people read and click: `title`, `body`, `body-strong`, `small`.
- **Martian Mono** is the machine's voice: branch names, commands, log lines, refused actions (`code`), numbers in
  tables (`data`, tabular figures) and the engraved uppercase labels on lamps and column heads (`label`, 0.08em
  letter spacing).

Keep running text at 60 to 70 characters per line. Headings use `text-wrap: balance`.

## Shape, space and depth

- Square-cornered equipment: `radius-sm` (2px) for tags and inputs, `radius-md` (4px) for buttons and panels.
  `radius-lamp` only for lamps and person markers. Nothing else is round.
- A 4px base: `space-1` to `space-8` (4 to 64px). Table cells use 8px by 12px; panels `space-4`; sections `space-6`.
- Depth is a machined edge, not a float: `shadow-panel` under panels, `shadow-dialog` only on dialogs, menus and
  overlays. Separate table rows with `line`, not with stripes.
- Focus: a 2px solid `focus` ring with a 2px offset, on every interactive element.

## The mark

A box with one opening and a lit lamp inside: the agent works in its sandbox, and the work leaves only through the
gate. Petrol box (the person's boundary), amber lamp and exit line (the agent).

- `agentdev-mark.svg` on light grounds, `agentdev-mark-dark.svg` on dark grounds, `favicon.svg` (dark tile) for
  browser tabs and app icons in both themes. The files have fixed inks; pick the file by the ground. Inline in a
  page, the mark can use the tokens instead (stroke `--gate`, fill `--lamp`, edge and exit line `--lamp-ink`).
- Minimum size 16px. Clear space: one lamp diameter on each side.
- The wordmark is the word `agentdev` set live in `display` 800, lowercase, in `ink`, beside the mark. There is no
  outlined wordmark file.

## Icons

Icons are rare. Use them only where a person decides or where something can go wrong: a create action, Approve and
Deny, Resume, a warning, a security entry point, an expiry time, a link to another page. Edit, Delete, Save and
table rows stay text only.

- Set: [Lucide](https://lucide.dev) outlines, 16px, about 1.5px stroke (`stroke-width` 2.2 in the 24px viewBox),
  round caps and joins, `stroke: currentColor` so the icon takes the colour of its text.
- In a button the icon goes before the label with a 6px gap; a "go to" arrow goes after it.
- No filled or multicolour icons, no emoji, no robot or face illustrations.

## Motion

Only one thing moves: a `starting` lamp pulses (1.2 s, opacity). Panels and pages do not animate in. With
`prefers-reduced-motion`, the lamp holds at half brightness.

## Components

All classes are in `components.css`. Markup is plain HTML, so React, Astro or static pages can use it.

**Button.** `ad-btn` plus one of `ad-btn-primary` (gate, at most one per view, for the decision the view exists
for), `ad-btn-danger` (only inside a confirmation for Delete), `ad-btn-quiet` (in-line actions such as Sign out).
No modifier gives the secondary button.

```html
<button class="ad-btn ad-btn-primary" type="button">Approve</button>
<button class="ad-btn" type="button">Cancel</button>
```

**Tabs.** The console navigation. The active tab has a gate underline; `ad-count` is a gate badge for things that
wait for a person (pending approvals), never for agent activity.

```html
<nav class="ad-tabs" role="tablist">
  <button class="ad-tab ad-tab-on" role="tab" aria-selected="true">Agents</button>
  <button class="ad-tab" role="tab">Approvals <span class="ad-count" aria-label="2 waiting for you">2</span></button>
</nav>
```

**Status lamp.** An agent's state as the operator reports it. Each state has its own shape and word:
`running` (lit amber), `starting` (pulsing), `stopped` (empty ring: scaled to zero, the normal idle state, not an
error), `blocked` (red square), `paused` and `usage-paused` (pause bars), `invalid` (struck square).

```html
<span class="ad-lamp ad-lamp-running"><span class="ad-lamp-dot"></span><span class="ad-label">Running</span></span>
```

**Severity tag.** The severity of a security event: `ad-sev-high`, `ad-sev-medium`, `ad-sev-low`. The word is always
shown.

```html
<span class="ad-tag ad-sev-high"><span class="ad-label">high</span></span>
```

**Agent tag.** Who did something. An agent: a square in its lane hue and a mono name. A person: a gate disc and a
sans name (`ad-person`).

```html
<span class="ad-agent ad-lane-3"><span class="ad-agent-mark"></span><span class="ad-agent-name">alice</span></span>
<span class="ad-agent ad-person"><span class="ad-agent-mark"></span><span class="ad-agent-name">valentinalexeev</span></span>
```

**Usage meter.** How much of a Claude subscription window is used. Amber fill, a tick at the limit; at or over the
limit, `ad-meter-over` turns it to `paused` and the text says "paused".

```html
<div class="ad-meter">
  <div class="ad-meter-head"><span class="ad-label">5-hour</span><span class="ad-data">64%</span></div>
  <div class="ad-meter-track" role="meter" aria-valuenow="64" aria-valuemin="0" aria-valuemax="100" aria-label="5-hour">
    <div class="ad-meter-fill" style="width:64%"></div><div class="ad-meter-limit" style="left:90%"></div>
  </div>
  <div class="ad-meter-foot">Resets 14:00 · limit 90%</div>
</div>
```

**Work card.** One issue as an agent works it, as on botboard: a four-step track (plan, build, review, done). Plan and
build are amber (the agent's turn), review is gate (your turn, and the card gets a gate edge with `ad-card-review`),
done is `ok`. A blocked card shows a red step and a note.

```html
<article class="ad-card ad-card-review">
  <div class="ad-card-top"><span class="ad-data">#173</span><!-- agent tag --></div>
  <h3 class="ad-card-title">One disk per agent</h3>
  <ol class="ad-track">
    <li class="ad-step ad-step-plan ad-step-past"><span class="ad-label">plan</span></li>
    <li class="ad-step ad-step-build ad-step-past"><span class="ad-label">build</span></li>
    <li class="ad-step ad-step-review ad-step-now"><span class="ad-label">review</span></li>
    <li class="ad-step ad-step-done ad-step-next"><span class="ad-label">done</span></li>
  </ol>
</article>
```

**Loop strip.** The product loop for the website hero and the docs: issue, branch, pull request, review, merge. The
top rule of each step names the actor (`ad-loop-agent` amber, `ad-loop-person` petrol). Once per page.

```html
<ol class="ad-loop">
  <li class="ad-loop-step ad-loop-person"><span class="ad-loop-who ad-label">person</span><span class="ad-loop-k">issue</span>
    <span class="ad-loop-d">You file it and assign an agent.</span></li>
  <li class="ad-loop-step ad-loop-agent"><span class="ad-loop-who ad-label">agent</span><span class="ad-loop-k">branch</span></li>
</ol>
```

## The management UI

The UI in `images/ui` moves to the system like this (planned, not done yet):

| Today | In the design system |
|---|---|
| `.phase.running / starting / stopped / blocked / paused / usage-paused / invalid` | status lamp, same state names |
| `.sev.high / medium / low` | severity tag |
| agent names in tables | agent tag |
| `button` (black), `.secondary`, `.danger`, `.link` | `ad-btn-primary`, `ad-btn`, `ad-btn-danger`, `ad-btn-quiet` |
| `button.tab` and the pending badge | tabs with `ad-count` |
| usage percent cells | usage meter |
| page `h1` | `display-3` |
| approval details dialog | an overlay panel over the table, gate edge, `shadow-dialog`, closes with Escape |
| Vite default favicon | `favicon.svg` |

Layout: a top bar on `panel` with the mark, the wordmark and the tabs; content on `ground`, at most 1200px wide;
tables on `panel` with a `sunken` header row of `label` text.

## The website

The product website lives in a separate private repository and **vendors** a copy of `docs/design-system/`, pinned to
an agentdev commit or release tag. Change the system here first; the website then updates its copy.

- The hero states the thesis in `display-1`: "agents propose. people merge." Under it, the loop strip.
- Each product section sits on `panel`: its name in `display-2`, one sentence of what it does, one real screenshot or
  live component, and a `code` block with the real command.
- Real numbers only, from these docs: "15 minutes idle, then the agent stops", "one disk per agent", "wake poll every
  300 s".
- Section padding `space-8` on desktop, `space-7` on phones. Text at most 680px wide; media up to 1080px.

## Changing the system

1. Edit `design-system/tokens.json` (keep a usage note on every token) or `components.css`.
2. Run `node scripts/design-tokens.mjs` and commit `tokens.css` with the change.
3. Check text contrast in both themes (4.5:1, or 3:1 for borders, focus and marks).
4. Update this page and add a `CHANGELOG.md` line.
