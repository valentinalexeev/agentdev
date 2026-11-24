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
| `design-system/console.css` | the console layer: shell, inbox, tables, drawer, untrusted text, board | yes |
| `design-system/console-prototype.html` | every console screen with example data, built only from these files | yes |
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

**Agent identity.** Six hues cannot tell 30 agents apart: the name is the identity, and the hue only helps inside one project. Each agent gets one of six lane hues (`lane-1` to `lane-6`), picked by a hash of its name, so an
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

**Button.** `agd-btn` plus one of `agd-btn-primary` (gate, at most one per view, for the decision the view exists
for), `agd-btn-danger` (only inside a confirmation for Delete), `agd-btn-quiet` (in-line actions such as Sign out).
No modifier gives the secondary button.

```html
<button class="agd-btn agd-btn-primary" type="button">Approve</button>
<button class="agd-btn" type="button">Cancel</button>
```

**Tabs.** The console navigation. The active tab has a gate underline; `agd-count` is a gate badge for things that
wait for a person (pending approvals), never for agent activity.

```html
<nav class="agd-tabs" role="tablist">
  <button class="agd-tab agd-tab-on" role="tab" aria-selected="true">Agents</button>
  <button class="agd-tab" role="tab">Approvals <span class="agd-count" aria-label="2 waiting for you">2</span></button>
</nav>
```

**Status lamp.** An agent's state as the operator reports it. Each state has its own shape and word:
`running` (lit amber), `starting` (pulsing), `stopped` (empty ring: scaled to zero, the normal idle state, not an
error), `blocked` (red square), `paused` and `usage-paused` (pause bars), `invalid` (struck square).

```html
<span class="agd-lamp agd-lamp-running"><span class="agd-lamp-dot"></span><span class="agd-label">Running</span></span>
```

**Severity tag.** The severity of a security event: `agd-sev-high`, `agd-sev-medium`, `agd-sev-low`. The word is always
shown.

```html
<span class="agd-tag agd-sev-high"><span class="agd-label">high</span></span>
```

**Agent tag.** Who did something. An agent: a square in its lane hue and a mono name. A person: a gate disc and a
sans name (`agd-person`).

```html
<span class="agd-agent agd-lane-3"><span class="agd-agent-mark"></span><span class="agd-agent-name">alice</span></span>
<span class="agd-agent agd-person"><span class="agd-agent-mark"></span><span class="agd-agent-name">valentinalexeev</span></span>
```

**Usage meter.** How much of a Claude subscription window is used. Amber fill, a tick at the limit; at or over the
limit, `agd-meter-over` turns it to `paused` and the text says "paused".

```html
<div class="agd-meter">
  <div class="agd-meter-head"><span class="agd-label">5-hour</span><span class="agd-data">64%</span></div>
  <div class="agd-meter-track" role="meter" aria-valuenow="64" aria-valuemin="0" aria-valuemax="100" aria-label="5-hour">
    <div class="agd-meter-fill" style="width:64%"></div><div class="agd-meter-limit" style="left:90%"></div>
  </div>
  <div class="agd-meter-foot">Resets 14:00 · limit 90%</div>
</div>
```

**Work card.** One issue as an agent works it, as on botboard: a four-step track (plan, build, review, done). Plan and
build are amber (the agent's turn), review is gate (your turn, and the card gets a gate edge with `agd-card-review`),
done is `ok`. A blocked card shows a red step and a note.

```html
<article class="agd-card agd-card-review">
  <div class="agd-card-top"><span class="agd-data">#173</span><!-- agent tag --></div>
  <h3 class="agd-card-title">One disk per agent</h3>
  <ol class="agd-track">
    <li class="agd-step agd-step-plan agd-step-past"><span class="agd-label">plan</span></li>
    <li class="agd-step agd-step-build agd-step-past"><span class="agd-label">build</span></li>
    <li class="agd-step agd-step-review agd-step-now"><span class="agd-label">review</span></li>
    <li class="agd-step agd-step-done agd-step-next"><span class="agd-label">done</span></li>
  </ol>
</article>
```

**Loop strip.** The product loop for the website hero and the docs: issue, branch, pull request, review, merge. The
top rule of each step names the actor (`agd-loop-agent` amber, `agd-loop-person` petrol). Once per page.

```html
<ol class="agd-loop">
  <li class="agd-loop-step agd-loop-person"><span class="agd-loop-who agd-label">person</span><span class="agd-loop-k">issue</span>
    <span class="agd-loop-d">You file it and assign an agent.</span></li>
  <li class="agd-loop-step agd-loop-agent"><span class="agd-loop-who agd-label">agent</span><span class="agd-loop-k">branch</span></li>
</ol>
```

## The management UI

The console design (activities, daily use, information architecture, screens, phases) is in
[Console UX](console-ux.md). Its components are in `design-system/console.css`, on top of `components.css`, and
`design-system/console-prototype.html` shows every screen with example data (open it from the folder, or see the
published prototype linked from the pull request).

Load order in the console: `fonts.css`, `tokens.css`, `components.css`, `console.css`.

### Two axes of agent state

An agent has a **lifecycle** (status lamp: running, starting, idle, paused, invalid) and an **attention** state
(`ok`, `needs you`, `blocked`, `guardrail`, `usage limit`). They are shown side by side, never merged into one
colour. The API's `stopped` (zero pods) is shown as **idle** with an empty lamp: it is the normal state of an agent
with no work, not an error.

### Console components (`console.css`)

| Component | Classes | Rule |
|---|---|---|
| App shell | `agd-shell`, `agd-rail`, `agd-topbar`, `agd-main` | Left rail on `panel` with the mark, navigation and counts; top bar with the page name (`agd-crumbs`, the page `h1`, #521), then the project picker, search, connection state, theme, user. Below 760px the rail becomes `agd-bottombar`. |
| Navigation item | `agd-nav`, `aria-current="page"` | Gate tint and a gate edge on the current page. `agd-count` only for things that wait for a person. |
| Connection state | `agd-conn`, `-stale`, `-waking`, `-down` | Always in the top bar. Not live adds a banner. Keep the last data and say its age (#339). |
| Banner | `agd-banner`, `-warn`, `-down`, `-paused` | One per cause, under the top bar, with one action. Never more than two. |
| KPI tile | `agd-kpis`, `agd-kpi`, `-you`, `-alert` | Only numbers a person acts on, each with its unit and a foot line. `-you` (gate) for "waiting for you". |
| Inbox | `agd-inbox`, `agd-item`, `-alert`, `-agent` | Things that wait for a person. A reason chip, the agent tag, where, and the expiry or age on the right. |
| Data table | `agd-table-wrap`, `agd-table`, `agd-num` | `sunken` header of `label` text; numbers right-aligned in mono; the first cell is a button that opens the drawer. |
| Filters | `agd-filters`, `agd-chip`, `agd-seg` | Chips for filters with counts, segmented control for one-of choices (time range, view). |
| Field | `agd-field`, `agd-input`, `agd-error` | Label above, help below, `aria-invalid` for errors. |
| Drawer | `agd-drawer`, `-gate`, `agd-scrim` | Detail beside the list so the list keeps its place. `-gate` when the drawer holds a decision. Full screen on phones. |
| Untrusted text | `agd-untrusted`, `agd-untrusted-head` | Everything the model wrote: plain text only, never HTML or a link, a hatched lamp-ink edge, the words "Written by the agent · not checked". |
| Timeline | `agd-timeline`, `agd-tl-agent`, `-person`, `-alert` | Square amber mark for the agent, round gate mark for a person, red for a refusal. Newest first. |
| Board | `agd-board`, `agd-col-plan / build / review / done` | Work cards by phase; the review column is the person's turn. One column per screen on phones. |
| Empty state | `agd-empty` | Says what will appear and how to make it appear. |
| Toast | `agd-toast` | The result of an action in one sentence: "Paused alice. Runs in progress finish." |
| Health lamp | `agd-health`, `-ok / -degraded / -down / -zero` | Platform parts only (API, router, operator). Green means healthy. Agents never use it: their lamp is amber because amber is the agent, and a running agent is activity, not health. |
| Chat layout | `agd-chat`, `agd-chat-list`, `agd-sess`, `agd-thread`, `agd-shell-fill` | Session list and thread; the page is one screen high (`agd-shell-fill` on the shell). The list folds behind a button below 1000px. |
| Message | `agd-msg`, `agd-msg-person`, `agd-msg-text` | The person: gate-tinted block on the right. The session: full width with the agent tag. Model Markdown with raw HTML off; links print their address. |
| Tool step | `agd-tool` (`<details>`) | One line per step the session took; the output opens in the untrusted block. |
| Permission card | `agd-ask`, `-done` | Claude Code asks before a command. Allow once or Deny; the answer and the name stay. |
| Issue draft | `agd-draft`, `-done` | The only path from a session to work: the session drafts, the person creates and assigns. Gate edge while it waits, ok edge when created. |
| Composer | `agd-composer`, `agd-ctx`, `agd-working` | Context chips, the text box, Send; while the session works, a pulsing lamp and Stop. |
| Command palette | `agd-palette`, `agd-kbd` | `Ctrl+K` or `/`. Pages, projects, agents and commands. No one-key approve anywhere. |
| Project scope | `agd-scope` | Left of the top bar: the projects every page shows and their count. Opens a palette of projects, sorted by what waits. |
| Group header | `agd-group-head` (lists), `agd-group-row` and `agd-group-btn` (tables) | A list groups itself above about 10 items. The header has the name, the urgency and the count. A table group folds; `aria-expanded` on the button. |
| Show more | `agd-showmore` | The end of a capped group. Always says how many are hidden. |
| Agent dots | `agd-dots`, `agd-d-run / -you / -alert / -paused` | One dot per agent of a project, in one line. Round = lifecycle, square = attention. Never the only signal: the row also has the counts. |
| Wall | `agd-wall`, `agd-tile`, `-you / -alert / -usage` | One tile per agent, attention first. For a fleet glance at 20 and more agents. |
| Phase matrix | `agd-matrix`, `agd-cell`, `-you / -alert / -agent / -zero` | Projects by work phase. A cell is a count and a link; review cells take the gate tint. Replaces the board when a scope has several projects. |

### Decisions in the console

- A decision (approve, deny, resume after a guardrail, delete) always opens its detail first. The trusted facts
  come from the platform and are above the untrusted text. The confirm step states the effect in one sentence and
  that the name goes to the audit log.
- Only the decisions the request type allows are buttons (no "Approve for the job" for an executor).
- No approval by timeout. The UI says what happens without a decision.
- A push notification never contains model-written text.

### How `images/ui` loads the system

`images/ui` has no colour values of its own. `npm run sync-design-system` copies `tokens.css`, `components.css`,
`console.css` and `favicon.svg` into `images/ui/src/design-system/` and `images/ui/public/`. The copies are committed,
because the image build sees only `images/ui`. `npm run build` fails when a copy differs from `docs/design-system`.
The fonts come from the `@fontsource` packages (self-hosted), because the console CSP allows no third-party styles.
Classes the console needs beyond the prototype (`agd-attn`, `agd-reason`, `agd-iconbtn`, `agd-user`, `agd-limit-row`,
`agd-meters`, `agd-sub`, `dialog.agd-drawer`) are at the end of `console.css`.

### Moving `images/ui` to the system

| Today | In the design system |
|---|---|
| `.phase.running / starting / stopped / blocked / paused / usage-paused / invalid` | status lamp (lifecycle) plus attention chip |
| `.sev.high / medium / low` | severity tag |
| agent names in tables | agent tag |
| `button` (black), `.secondary`, `.danger`, `.link` | `agd-btn-primary`, `agd-btn`, `agd-btn-danger`, `agd-btn-quiet` |
| `button.tab` and the pending badge | rail navigation with `agd-count` |
| usage percent cells | usage meter |
| page `h1` | `agd-crumbs` in the top bar (#521; `agd-page-title` is no longer used by the console) |
| approval details dialog | `agd-drawer-gate` with `agd-untrusted` |
| `pre.untrusted` | `agd-untrusted` |
| `.banner` | `agd-banner` |
| Vite default favicon | `favicon.svg` |

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
