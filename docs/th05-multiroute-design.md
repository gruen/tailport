# tailport th05 — Record/Route visual rework (design spec, v0.2.1)

Status: ACCEPTED design from the fable design pass (kata th05). Owner-signed-off;
§7's open questions were resolved with the recommended default in each case
(see §7 below). Implemented under kata th05.

## 0. The one-sentence move

The glyphs keep their identities; their **attachment point** changes. The old
aggregate marker was "the widest route" — so every existing glyph already names
a route type. We stop collapsing: each route line carries its own glyph, and the
record header carries **no state marker at all**. Identity (port, name, ★, 🔒)
lives on the header; reachability lives on the routes.

## 1. Per-route marker table

Shape encodes tier, color reinforces (colorblind-safe): private routes are
ring-ramp circles (outline → filled); public routes are solid glyphs with three
distinct shapes (circle / diamond / lozenge). Broken states sit off the ramp.

| Route type | Mono | Color (Light / Dark) | Contrast L/D | Emoji | Disposition |
|---|---|---|---|---|---|
| `localhost` | `○` | muted `{L:#4b4b4b, D:245}` | 8.7 / 6.1 | `🌕` | kept; demoted to muted (§6) |
| `LAN` (specific LAN-IP bind) | `◔` | default fg | n/a | `🌔` | kept |
| `tailnet` — via wide bind | `◑` | green `{L:#006644, D:42}` | 7.0 / 11.1 | `🌓` | **revived** (qptn merged into ◉; per-route lines give provenance room) |
| `tailnet` — via tailscale serve | `◉` bold | green `{L:#006644, D:42}` | 7.0 / 11.1 | `🌒` | kept |
| `ts.net` (funnel, public) | `●` bold | magenta `{L:#8b008b, D:201}` | 8.5 / 6.7 | `🌑` | kept |
| `caddy` (publish, public) | `◆` bold | blue `{L:#005fd7, D:39}` | 5.8 / 8.6 | `🌐` | kept |
| `cloudflare` (tunnel, public) | `◈` bold | orange `{L:#c2410c, D:208}` | 5.2 / 8.7 | `☁️` | kept |
| stale/dangling route | `▲` bold | amber `{L:#8a4500, D:214}` | 7.2 / 11.4 | `🌫️` | kept; now per-route |
| `offline` (down favorite) | `✕` | gray `{L:#4b4b4b, D:245}` | 8.7 / 6.1 | `✕` | kept; pseudo-route (§2) |

All pairs ≥4.5:1 on white and black. **Fix while here:** `lockStyle` is a fixed
`Color("208")` = #ff8700 on light = **2.4:1**; make it adaptive
`{L:#c2410c, D:208}`.

**Retired:** the aggregate-state role of every glyph; the "funnelled AND
published → ▲ drift" hack in `reach()` (coexistence is legitimate now);
`bindPrefix()` (§2); bubbles' default pink selected-item styling (§4).

## 2. Row anatomy

Record header carries **no aggregate marker** (single source of truth was the
point). `bindPrefix()` retired: the bind is expressed as routes (a wide bind →
`◑ tailnet` route, or a LAN route when tailscale is down; a specific LAN-IP bind
→ `◔ LAN` route whose URL carries the IP). Header shows only `:PORT`.

Fixed column grid, identical mono/emoji (marker field always 2 cells — §5). Zero
out bubbles' stock NormalTitle left-padding; the design owns column 0.

```
col ruler   0         1         2         3         4
            0123456789012345678901234567890123456789012345

HEADER      ▎★ :3000   vite 🔒
            ││ │       └─ col 11  name/label + " 🔒" trailing
            ││ └─ cols 3–8  ":PORT" left, 6 wide, bold
            │└─ col 1  record badge: ★ or space
            └─ col 0  record bar ▎ (current record) or space   (cols 9–10 gap)

ROUTE       ▎  ▸ ●  ts.net      https://myhost.tail1234.ts.net  ✓ copied
            │  │ │  └─ cols 8–17 route-type label, 10 wide (col 18+ URL at 20)
            │  │ └─ cols 5–6 marker field (2 cells)   (col 7 gap)
            │  └─ col 3 selection pointer ▸ or space
            └─ col 0 record bar; cols 1–2 indent
```

- Header: `★` yellow `{L:#8b6500, D:220}` col 1; port bold; name precedence as
  today (label → live process → italic-gray `was <proc>` → `?`); `🔒` after name.
- Route trailing adornments after URL, 2 spaces apart: auth `@`/`👤` (caddy
  color); `· stale` (amber, stale route); transient `✓ copied` (green bold,
  existing `copiedSuffix`). Keep `inlineCopyFits` → toast fallback; URLs
  truncate with `…`.
- Route order within a record (fixed): `localhost`, `LAN`, `tailnet`, `ts.net`,
  `caddy`, `cloudflare` — narrow → wide, so blocks "end hot."
- Labels lowercase except `LAN`; field 10 wide; future types ≤10 chars.
- Every record has ≥1 sub-row (nav invariant). Down favorite → one `✕ offline`
  pseudo-route with `—` URL (`c` → error toast). Stale serve → its `▲` route
  (no localhost route; nothing listening).
- Non-HTTP: `:22` → `ssh myhost` / `ssh localhost`; unknown → `host:port`.
- Wildcard binds get NO LAN route line (noise in a tailnet tool).
- Records separated by one blank line; none within a record.

## 3. ASCII mockups

### 3a. Mono — route-selected (current record `vite`; selected `ts.net`, just copied)

```
▎★ :3000   vite
▎    ○  localhost   http://localhost:3000
▎    ◉  tailnet     http://myhost:3000
▎  ▸ ●  ts.net      https://myhost.tail1234.ts.net  ✓ copied
▎    ◆  caddy       https://app.example.com  @
▎    ◈  cloudflare  https://witty-fox-42.trycloudflare.com

  :5173   astro
     ○  localhost   http://localhost:5173

  :22     sshd 🔒
     ○  localhost   ssh localhost
     ◑  tailnet     ssh myhost

  :8080   api
     ○  localhost   http://localhost:8080
     ◉  tailnet     http://myhost:8080

  :5432   postgres
     ◔  LAN         192.168.1.5:5432

  :8025   was mailpit
     ▲  tailnet     http://myhost:8025  · stale

 ★ :6379   was redis-server
     ✕  offline     —

 ↑↓/jk route · ⇧↑↓/JK service · c copy route · space serve · f funnel · ? help
```

### 3b. Mono — after one shift-jump down (`J`): landed on `astro`'s first route

```
 ★ :3000   vite
     ○  localhost   http://localhost:3000
     ◉  tailnet     http://myhost:3000
     ●  ts.net      https://myhost.tail1234.ts.net
     ◆  caddy       https://app.example.com  @
     ◈  cloudflare  https://witty-fox-42.trycloudflare.com

▎  :5173   astro
▎  ▸ ○  localhost   http://localhost:5173

  :22     sshd 🔒
     ○  localhost   ssh localhost
     ◑  tailnet     ssh myhost
```

### 3c. Emoji — route-selected (same grid; marker field 2 cells in both modes)

```
▎★ :3000   vite
▎    🌕 localhost   http://localhost:3000
▎    🌒 tailnet     http://myhost:3000
▎  ▸ 🌑 ts.net      https://myhost.tail1234.ts.net  ✓ copied
▎    🌐 caddy       https://app.example.com  👤
▎    ☁️ cloudflare  https://witty-fox-42.trycloudflare.com

  :5173   astro
     🌕 localhost   http://localhost:5173

  :8025   was mailpit
     🌫️ tailnet     http://myhost:8025  · stale

 ★ :6379   was redis-server
     ✕  offline     —
```

**Color legend (not ASCII-renderable):** `▎`/`▸`/selected URL = accent teal;
`◉◑` green; `●` magenta; `◆` blue; `◈` orange; `▲`+`· stale` amber; `★` yellow;
`○`, localhost URLs, `✕`, `—`, `was …` = muted gray; public route labels take
their marker's color, bold; `✓ copied` green bold.

## 4. Selection and navigation

**Accent color (new): teal `{L:#005f5f, D:51}`** — the existing `logoStyle` pair
(7.5:1 / 16.7:1). Replaces bubbles' default pink (documented as colliding with
funnel magenta, e0e7/ze1z). Three redundant channels:

1. **Selected route** — `▸` pointer col 3 (accent, bold) + URL **bold +
   underline** (+ accent fg when available). Route-type label keeps its own
   color; the marker is **never restyled** by selection (safety colors survive).
2. **Current record** — `▎` bar (U+258E, accent) down col 0 of every line of the
   record incl. header; header port also accent.
3. **No color / dumb terminal** — pointer + bar + bold/underline carry it.

**Keys:** `↑/↓` and `j/k` walk the flattened route list across all records;
`shift+↑/↓` and `J/K` jump to the first route of the prev/next record. Headers
never selectable. `c`/`y` copy the selected route's address, inline `✓ copied` on
that line — retires the funnel toast exception (`inlineCopyState`): shown==copied
for every route now. All other actions act on the **parent record** regardless of
selected route; contextual bottom-bar hints follow the selected route (stale `▲`
route → `space unbind`).

## 5. Marker modes

- `mono`/`ascii`: identical set (1-cell Unicode; keep the alias).
- `emoji`: moon ramp + 🌐/☁️/🌫️, 👤 auth.
- **Marker field is 2 cells in every mode** (mono pads 1-cell glyph; emoji keeps
  the `lipgloss.Width` pad loop) so label/URL columns never move across modes.
- `★ ▸ ▎ ✓ 🔒` mode-independent.

## 6. Scanning and density

"Which services are public right now?" — three stacked cues:
1. **Marker rail** — all markers in one fixed column (col 5); public = solid,
   saturated, bold (`● ◆ ◈`); private = muted/default circles.
2. **Block height ∝ exposure** (narrow→wide ordering; tall blocks ending in
   solid glyphs = more exposed).
3. Optional recommended: a `● N public` tally in the top bar.

**Quiet single-route services:** the whole `localhost` route line renders muted
(`{L:#4b4b4b, D:245}`), so a localhost-only service is bold-header + muted-route
= 2 lines, exactly today's title+description height (zero density cost).

## 7. Rationale and resolutions

Key choices: no aggregate header marker (one glyph = one meaning = one place);
`bindPrefix` retired (LAN IP moves into the LAN route address); `◑`/`◉` kept
separate (provenance decides whether `space` unbind applies); existing
glyph/emoji vocabulary preserved 1:1; teal accent kills the magenta
selected-vs-funnel tension.

**Resolved by the owner (kata th05):** the five open questions below were
signed off with the recommended default called out in each case; the th05
implementation follows these resolutions.

1. **Grid view** (`renderGrid`): cells are one-per-service; proposal = render the
   record's marker strip (`○◉●◆◈`) instead of one aggregate glyph. **Resolved:
   deferred** — a separate mini-design, out of scope for th05; `renderGrid` is
   unchanged.
2. **Shift-jump landing**: first route of target record (simple/predictable) vs
   remember last-selected route per record. **Resolved: first route of the
   target record** — the simple/predictable option; shift-jump always resets
   the route cursor to 0 on the record it lands on.
3. **One-line merge for single-route records** would halve the common case's
   height but breaks the uniform grid/route-selection model; rejected here, noted
   as the density lever if lists feel tall. **Resolved: no merge** — every
   record keeps its route sub-row(s); kept in reserve as the future density
   lever, not adopted for th05.
4. **Per-route actions**: copy is route-scoped now; `space`-unbind and a future
   "remove this caddy route" arguably want route scope too once multiple public
   routes coexist. **Resolved: out of scope for th05** — only `c`/`y` copy is
   route-scoped; every other action (`space`, `P`, `p`, `t`, `x`, `e`, …) stays
   scoped to the parent service/record, not the selected route.
5. **"localhost always present"**: this spec exempts strict LAN-IP binds (no
   loopback listener exists, e.g. the postgres mockup). **Resolved: confirmed**
   — a strict LAN-IP bind gets no `localhost` route; the route derivation only
   emits one for a loopback or wildcard bind.
