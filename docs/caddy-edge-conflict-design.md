# Design: Edge route conflict resolution — refuse, force-purge, poof, undo

**Status:** **rev 2 — SIGNED OFF (mg, 2026-08-28).** All open questions resolved below (§0). Build proceeds A→B in sequence (`qfbf`→`6n15`→`dw57`→`ttfh`).
**Kata:** epic `9jbr`; design gate `2vn2` (this doc); blocks impl subissues `qfbf`/`6n15`/`dw57`/`ttfh`.
**Relates to:** `v1z5` (shipped publish path), `ycv1`/`w131` (auto-config + domain capture — landed), `5x1e` (fireworks ticker infra).
**Touches no shipped design constraint** (adds behavior branching off an error the shipped `Publish` already returns). The remote-undo category is genuinely new and is where the danger lives.

> **Headline for sign-off:** two independent adversarial rounds converged on one recommendation — **ship pillar 1 (refuse-with-attribution) first; treat force-purge + poof + undo as a gated fast-follow (OQ6).** Pillar 1 needs **zero** new `caddyedge` primitives and has none of the failure modes below. The remote-undo story (pillar 4) is a genuine hazard: round 1 found 1 blocker + 6 major issues in it, all rooted in one structural error (a fixed message-table where the real edge state is a *product* of outcomes). The Caddy admin-API mechanics the purge rests on are now **source-verified** (good news), but they carry a **version floor** that must be stated.

---

## 1. Problem

A `P` publish takes a public hostname over the Caddy edge — a **shared** routes array every tailport machine on the tailnet writes into, and which can also carry hand-authored routes. So a publish can collide with a hostname the edge already holds. `caddyedge.Publish` (caddyedge.go:474) already **refuses** every such collision with `ErrHostnameConflict`, mutating nothing — three sub-cases:

1. our `@id` exists but points at a **different backend/port** (caddyedge.go:496–499);
2. our `@id` exists but its host matcher was **hijacked** by a foreign edit (`hostMatcherIs` fails, caddyedge.go:488–491);
3. no `@id` of ours, but a **foreign route** already claims the host (`findHostConflict`, caddyedge.go:520–522, 686–700).

Today the UI dumps that failure as a bare toast (`publishErrText` default branch, ui.go:1547–1550): the user sees Caddy's message but gets no clean attribution and no way forward except editing Caddy by hand. Note (review r2-m1): Publish's error **already carries** the attribution — the owned case names the backend (`"%q is published to %s:%d…"`, caddyedge.go:497) and the foreign case names the route id (`"route <id>"`, caddyedge.go:691). So pillar 1 is mostly a UI-formatting job over an error we already have.

The signed-off epic (`9jbr`, from ycv1 OQ4) asks for four things:

1. **Refuse-on-conflict, clearly** — name the current owner/backend in the refusal.
2. **Force-purge override** — capture the offending route's JSON, delete it, take over. Tailport-**owned** conflict → **normal** confirm; **foreign** route (drift; AGENTS.md — *never silently overridden*) → **scary second** confirm.
3. **"poof"** — an ASCII delete animation, built on the decoupled fireworks-ticker discipline (`5x1e`).
4. **[u]ndo of a purge** — a **new remote-undo category** (today's `u`/`undoStack` is local-registry-only + session-only, ui.go:778–789): re-POST the captured route JSON, with honest "couldn't restore" handling.

**The load-bearing risk is pillar 4**: undo mutates shared remote state that keeps moving under us. Most of this document is spent breaking it (§3.6).

## 2. Principles

1. **Never silently override drift.** A route tailport didn't create is drift (AGENTS.md); purging one is only ever behind a deliberate, escalated confirm.
2. **Caddy is the source of truth.** Classification is read live at decision time, never inferred from `m.published` (owned-and-this-machine-only, ui.go:1502). Every mutation re-verifies live before acting — the discipline shipped `Publish`/`Unpublish` already follow.
3. **Capture must be byte-faithful.** Undo replays the *exact bytes* Caddy held, not a lossy re-serialization (§3.3).
4. **A purge never touches serve/funnel.** It deletes one *remote* route for one hostname; it never flips this machine's serve/funnel state, and never collapses the "funnelled AND published" drift marker (ui.go:553).
5. **Honesty over cleverness in undo.** Undo is best-effort against a world that moves. **It computes what it says from a final re-read of the edge, never from a guess about which step failed** (§3.6 — this is rev-2's central correction). A confidently-wrong "restored" is worse than an honest "couldn't."
6. **Reducing exposure is never gated; increasing it always is.** Purge+takeover is gated. Undo re-creates someone else's exposure and removes ours — offered, never automatic, never silent.

## 0. Decisions (mg sign-off, 2026-08-28)

- **OQ6 → build all four pillars, sequenced A→B** (they're coupled and relatively small). Not deferring B behind 9kgt; A (`qfbf`, refuse-with-attribution) lands first, then B (`6n15`/`dw57`/`ttfh`).
- **OQ8 → undo is OWNED-purge-only.** A foreign purge is deliberate and **one-way — no undo offered**. Re-creating a route a human authored would re-create the very drift AGENTS.md says tailport never silently touches. **This dissolves OQ5 entirely** (the id-less-foreign order-fidelity loss only arose when restoring a foreign route, which no longer happens).
- **OQ2 → A-before-B** (remove our takeover, then restore), endorsed by both reviewers.
- **OQ3 → a DISTINCT restore key** shown in the action line — not a contextual `u` (which shadows the registry-undo key and enables a `u,u` footgun). Registry `u` gains a one-line pointer.
- **OQ1 → ~60s lifetime + competing-action clears** (default; the slot also clears on restore / new non-takeover publish or purge / third-party re-take / de-escalation / navigation).
- **OQ4 → foreign scary confirm = two gates** (a `y/n` warning that names the drift, then a typed `purge` commit modeled on the SSH gate).
- **OQ7 → cross-machine owned takeover: keep the single normal confirm, but the message NAMES it as another machine's/user's route** (detectable via the backend-label-vs-`shortLabel(fqdn)` discriminator). Not escalated to the foreign ladder — it's still a tailport-owned route.
- **OQ-Poof → one transient status-slot action line** carrying poof→restore (no third unconditional sticky banner).
- **OQ-Serve → yes, flash on cancel-after-serve-enable** ("serve left on for :port — space to stop").
- **OQ-Version → yes, document a Caddy ≥ 2.5.2 floor** in `docs/caddy-edge.md` (the concurrency model needs `If-Match`; shipped `caddy:2-alpine` already satisfies it).

*These resolutions are folded into the sections below; where a section still frames something as an OQ, §0 governs.*

## 3. Architecture

### 3.0 Where the branch lives + resuming the publish

The collision surfaces **only after the user confirms**, when `confirmPublish` → `publishCmd` → `client.Publish` returns `ErrHostnameConflict` as `publishDoneMsg{err}` (ui.go:2654–2668). So the conflict branch hangs off the **`publishDoneMsg` error handler**, *after* every local guard (busy/`:22`/empty-fqdn/funnel-mutual-exclusion/already-published/lock/hostname/domain) has passed — exactly right: `:22`, lock, and funnel-mutual-exclusion can never be bypassed by this flow.

**Resuming after a purge.** By `publishDoneMsg` time, `clearPublishFlow` has zeroed the flow (ui.go:2521). The plaintext password is *already* bcrypt-hashed into `cfg.Caddy.AuthHash` before the op runs (ui.go:2496–2513), so **no secret must survive** — carry a `pendingPublish{port, hostname, label, withAuth, enableServe}` and rebuild `auth` from `cfg.Caddy.AuthUser/AuthHash`. `enableServe` re-run is idempotent (serve already on). *(Both reviewers verified: no plaintext survives; the carry is sufficient.)*

**Pillar 1 is the floor and is independently shippable.** See OQ6 — it needs no new `caddyedge` primitive.

### 3.1 Detecting + classifying the conflict (read-only)

On `ErrHostnameConflict`, before any confirm, one **classification read**. We **cannot** use `List()` (it runs `parseRoute`, which *skips* any non-`reverse_proxy` route, caddyedge.go:452–457/590–597 — a foreign `static_response`/`file_server` would vanish), and we cannot use `m.published` (owned-this-machine-only). We need a **raw scan**.

New: **`caddyedge.InspectConflict(ctx, hostname) (ConflictInfo, error)`** —

```
ConflictInfo{                 // as shipped in qfbf
    Kind       ConflictKind   // None | OwnedDiffBackend | IdHijacked | ForeignOverlap
    Owned      bool           // @id carries tailport- prefix
    ID         string         // @id, "" if id-less foreign
    Label      string         // reverse_proxy dial label (a hostname/IP, hence string)
    Port       int            // reverse_proxy dial port
    BackendParseable bool     // false for a non-proxy foreign route (Handler names it)
    HijackedTo string         // for IdHijacked: where our @id now points
    Handler    string         // first handler name, for naming a non-proxy foreign route
}
```
It deliberately carries **no** capture bytes / array index / etag: capture and the delete are `PurgeConflict`'s job on its own fresh read (§3.2, review r1-F9 — a classify-time index/etag is a TOCTOU trap), which keeps the classifier lean and free of any raw-storage dependency.

**It must do TWO reads (review r2-M1 — the hijacked-`@id` hole):**
1. `fetchByID(IDFor(hostname))` — catches sub-case 2 (**our `@id` re-pointed to a *different, non-overlapping* host**). A pure host-overlap scan for the *requested* name would **miss** this (the route no longer matches our hostname), return `Found==false`, and the old "retry the plain publish" rule would **livelock** (Publish re-refuses on `hostMatcherIs` forever). So: if our `@id` exists but its matcher isn't ours → `Kind=IdHijacked` → a **refusal** ("your tailport route for `<host>` was re-pointed at `<other>` — resolve it in Caddy"), never a retry, never a blind `DELETE /id` (that would delete drift AGENTS.md forbids touching).
2. a **raw host-overlap scan** (reuse `hostsOverlap`) for sub-cases 1/3 and to name the holder.

If truly nothing overlaps and our `@id` is clean → `Kind=None` → retry the plain publish **once** (bounded, never a spin).

**Neither the raw bytes nor an array index live on `ConflictInfo`** (review r1-F9; corrected in qfbf): the delete's index, its raw capture, and its `If-Match` all come **solely** from `PurgeConflict`'s own fresh re-read (§3.2). A classify-time index/etag would be a TOCTOU trap, so the classifier deliberately doesn't carry one — `InspectConflict` only names the holder; `PurgeConflict` re-reads and captures.

### 3.2 The deletion mechanic — **SOURCE-VERIFIED against Caddy** (was: assumption)

Round 2 verified both halves against Caddy's own source/PR (primary):

- **`DELETE /config/.../routes/<index>` deletes-and-shifts.** `unsyncedConfigAccess` parses the trailing path part as a numeric index, bounds-checks it, and for DELETE reslices `append(arr[:idx], arr[idx+1:]...)` (caddy `admin.go`). Accepted, bounds-checked, shifts subsequent elements.
- **A parent-scope ETag guards a child-index DELETE.** The `If-Match` handler splits the header and **re-hashes the config at the path embedded in the `If-Match` value (`parts[0]`), independent of the request URL** (caddyserver/caddy **PR #4579** diff). So `DELETE .../routes/<i>` with `If-Match = "<…/routes> <hash>"` re-hashes the whole routes array; any concurrent add/remove/reorder moves the hash → **412** → re-read + retry. **Index-shift TOCTOU is defeated by construction.**
- ETag format is opaque to us: `do()` reads `Etag` and sets `If-Match` verbatim (caddyedge.go:360/374), so later quote-format changes (PR #4879) don't matter.

**BLOCKER B1 — version floor (review r2-B1).** ETag/`If-Match` first shipped in **Caddy v2.5.2**. On an **older** edge, Caddy **ignores `If-Match` entirely** — no 412 is ever returned, every retry loop (shipped `Publish`/`Unpublish` *and* the new purge) is dead code, and concurrent writers clobber blind. This must be **stated as a requirement: Caddy ≥ 2.5.2** (recommend ≥ 2.11.x for security hygiene — Caddy's admin `/config` index addressing has had parsing/authorization bugs historically; *exact CVE id unconfirmed, do not quote one without checking*). **Concrete status:** the shipped edge image pins `caddy:2-alpine` (Dockerfile:15), currently 2.x well above the floor — so a from-our-packaging edge is fine; the floor matters for a hand-run/older edge and belongs in `docs/caddy-edge.md`.

**Addressing:** prefer stable `@id` deletion where one exists (owned routes, and any foreign route carrying an `@id`); reserve index-DELETE for the **truly id-less foreign** route.

```
if id != "":  DELETE /id/<id>       If-Match=<id-scope etag>
else:         DELETE .../routes/<i> If-Match=<routes-array etag>   // id-less foreign only
```

**New primitive `PurgeConflict(ctx, hostname, expect) (Captured, error)`:** loop ≤ `maxRetries`: re-read the array (fresh etag) → re-locate the overlapping route → if none, `ErrNoConflict` → **re-verify it still matches `expect`**; if it changed (esp. **owned→foreign escalation**, or an **owned backend swap** — review r1-F8: pin the backend in `expect` for owned routes so a swap re-shows the confirm) → `ErrConflictChanged` (UI re-classifies + re-confirms with the correct ladder) → capture `Raw` → delete by `@id`/index under `If-Match` → 412 ⇒ retry; 2xx ⇒ `Captured{Raw, Hostname, HadID}`. New sentinels `ErrNoConflict`, `ErrConflictChanged`.

### 3.3 Capture must be raw bytes — **confirmed necessary** (both reviewers)

**Capture happens only for an OWNED purge** — the only kind that is undoable (OQ8; a foreign force-purge is one-way and captures nothing). But raw bytes are still the right capture even for an owned route: `Match` round-trips its matcher keys (caddyedge.go:123/147–152), yet **`Route`/`Handler` have no raw catch-all** (caddyedge.go:98–103/158–193, all `omitempty`), so a route decoded into `[]Route` and re-marshaled is **silently mutilated** if it carries any field tailport doesn't model. A tailport-`@id`'d route *usually* holds only modeled handlers (reverse_proxy + optional http_basic), but a foreign edit can add unmodeled fields to it while keeping the `@id` (the matcher guard `hostMatcherIs` does not police the handler chain). So capture keeps the exact `json.RawMessage` of the element and `RestoreRoute` POSTs *those bytes* — byte-faithful regardless of what the owned route accreted. (This is a weaker necessity than rev-1's foreign-`static_response` scenario, since we no longer restore foreign routes, but it is still the correct, robust choice.)

### 3.4 The confirm ladders

Reuse the two proven shapes: funnel-grade `y/n` (`entryConfirmFunnel`) and the typed-word gate (`entryConfirmUnlockSSH`, accepts only an exact word, ui.go:2944–2955).

- **Owned — one normal confirm** (`entryConfirmPurgeOwned`, y/n) — text **must name the backend** (`host-b:3000`) so a cross-machine takeover is never invisible.
- **Foreign — a scary second confirm** (two gates): a `y/n` warning that names the drift, then a **typed word** (`purge`) commit modeled on the SSH gate (exact, trimmed, case-insensitive; empty/enter cancels). Word choice + one-vs-two gates = **OQ4**.

**Cross-machine owned (review r2-m2, sharpens OQ7):** same-machine vs another machine's/user's publish **is** detectable (compare the conflict's backend label to `shortLabel(m.fqdn)`, the exact discriminator the poll uses at ui.go:1502). Taking over *another user's* owned route behind only the normal confirm is a quiet cross-user override — recommend at least a distinct message, arguably the scarier ladder. → OQ7.

### 3.5 The poof + the restore prompt — **one bottom-region "action line"** (revised per r1-F13, r2-M2/M3)

Rev-1 proposed the poof as a bottom line *and* a third sticky banner for restore. Both reviewers rejected that: three unconditional sticky banners cost every user a permanent list row (`bannerLines` is worst-cased even when off), dilute the "scary" signal, and the poof itself had **no reserved slot** → it would clip the list. And in this very flow up to **four** bottom lines could stack (operator + domain-setup + restore + poof).

**Rev-2 model:** the poof and the "press `<key>` to restore" prompt share **one transient bottom-region action line**, routed through the **live-measured status slot** (`statusLines := lipgloss.Height(m.renderStatusLine())`, ui.go:5588) that already self-reserves for `m.flash` — every mutation calls `resizeList`, so the reservation tracks it with no new worst-case constant and no third sticky banner. The poof animates the purged descriptor dissolving, then the same line becomes the restore prompt for the affordance's lifetime. *(Whether this is the right primitive is **OQ-Poof**, new.)*

**Ticker:** confirmed the poof **cannot** reuse `fwTickMsg` (its handler hard-stops on `!showEgg`, ui.go:2602–2608; a poof runs egg-closed). Add a **sibling** `poofTick()`/`poofTickMsg{}` + `stepPoof`, copying `fwTick`'s no-leak discipline, reusing the *glyph vars* `fwGlyphsUnicode/ASCII` (ui.go:4543) gated on `m.emoji` and `eggRampColor` (not the `*firework` methods). Both reviewers confirmed a sub-second sibling ticker is the right call (the 15s poll is far too coarse; relaxing the egg guard entangles the fireworks lag-clutch and is worse).

### 3.6 The remote-undo model — **rewritten: compute the message from a final re-read**

**This is rev-2's central change (round 1, F1–F4).** Rev-1's §3.6 was a flat table keyed on "which step failed." But the real post-undo edge state is a **product** of (step-A outcome) × (step-B outcome) × (who overlaps the name now), so table-row message selection emits confidently-wrong strings (announcing "now UNCLAIMED" when a third machine re-claimed it; "our route was already gone" when it's still live and now overlapping). **Fix: undo ends with a mandatory FULL-array re-read and classifies its message over the *observed* post-state** — `unclaimed` / `claimed-by-us` / `claimed-by-a-third-party` / `overlap` — never over a guess. **This final check must NOT reuse `InspectConflict` as-is** (roborev-k7br-#2): `InspectConflict` short-circuits on finding our `@id` with our matcher (it returns at read 1 without scanning the rest of the array), so it would miss an *additional* overlapping route appended between step B and the check and could falsely announce "restored". The final check is a distinct **full-array scan** that verifies BOTH our restored route is present AND no other route overlaps the hostname. (B's own scan-before-append already makes a between-B-and-C overlap a narrow window, but the check must still be a full scan to be honest.)

**Scope + lifetime — argued, not assumed.** A purge-undo is a **single-step, ephemeral, session-memory** affordance — **not** a persistent `undoStack` entry, **not** redoable — because: the local `undoStack` holds *registry deltas* applied to `cfg.Ports` and persisted (ui.go:1925/1981), redoable, `wouldUnlockSSH`-guarded — a remote mutation to shared state has none of those properties and the wrong lifetime; and the `u` help **promises** undo "does NOT touch what's exposed… undo never flips them" (ui.go:4952), which a purge-undo would falsify if folded in. One slot: **`m.lastPurge{captured, hostname, ourLabel, ourPort, hadID}`**.

**The mechanics (order is load-bearing — A before B):**
- (A) **remove our takeover** — `Unpublish(hostname, ourLabel, ourPort)` (re-verifies it's still ours + our backend, caddyedge.go:543).
- (B) **restore the captured bytes** — `RestoreRoute(ctx, captured)` = **scan-then-append under one array `If-Match`** (review roborev-ew0q-#1): read the routes array (fresh etag), scan it for a host-overlap with `<hostname>` **or** a duplicate `@id`, and if the name is **already claimed refuse** (do not append — Caddy permits overlapping host matchers, so a blind POST-append would silently create *dual exposure* the C-read could only report after the fact); only when the name is genuinely free, POST-append the raw bytes with **that same array etag** as `If-Match`. A concurrent append between the scan and the POST moves the array hash → 412 → re-read, re-scan, retry. So refuse-before-append under the array `If-Match` is atomic against the overlap race, rather than append-then-discover.
- (C) a final **full-array scan** (NOT `InspectConflict`, which short-circuits on our `@id` — roborev-k7br-#2) that confirms the resulting state for the message: our restored route present AND no other route overlapping the hostname (belt-and-suspenders with B's own scan-before-append).

**A-before-B** (confirmed sound by both reviewers): an owned captured route shares our takeover's `@id`, so B-first risks a duplicate-`@id`; A-first also guarantees no rogue duplicate of ours, and its failure window ("hostname briefly unclaimed") is safer than "two overlapping routes."

**Corrected failure handling (rev-2):**
- **Split A's outcomes** (r1-F1): `Unpublish → ErrNotFound` ⇒ our route is truly gone ⇒ safe to proceed to B. `Unpublish → ErrHostnameConflict` ⇒ **our route is still live** (matcher hijacked or backend changed; it *refused* to delete) ⇒ **do NOT append** (that creates overlap) — abort with "your takeover changed under you — resolve in Caddy; not restoring." (Rev-1 wrongly treated these as identical.)
- **B refuses on a re-claim, up front** (r1-F2/F3 + roborev-ew0q-#1): if B's scan finds the name re-claimed (a duplicate `@id` from a third machine, or an overlapping foreign route), it does **not** append — it reports "another route now claims `<host>` — resolve the drift in Caddy," leaving our takeover already removed by A. Only a genuinely free name is appended; only then, and only if the C-read confirms it empty-then-ours, do we say "restored."
- **Retryable modes re-arm** (r1-F11): on `ErrUnreachable` (A or B), keep the slot+banner and say "edge unreachable — press `<key>` to retry"; the ~60s clear timer is **suspended while a restore is in flight** and re-armed on a retryable outcome.
- **In-flight guard** (r1-F5): restore sets `m.pending` (or a `restoring` flag) on launch and refuses a second launch until `restoreDoneMsg` — the discipline every other remote op follows; without it, double-`<key>` fires concurrent A/B and can append the captured route twice.
- **Order-fidelity: content restored, position not (accepted loss — corrected per roborev-k7br-#2).** Owned-only undo removes rev-1's WORST case (an id-less foreign route restored by a now-meaningless array index — that never occurs, an owned route always has an `@id`). But an `@id` preserves a route's *content*, not its *array position*: `RestoreRoute` POST-**appends**, so the restored route lands at the array END, and Caddy evaluates routes in order — so its precedence relative to a catch-all or other non-overlapping route can differ from before the purge. In practice this is benign (tailport routes are host-specific and `Terminal:true`, so a more-specific host match still wins regardless of order), but it is a real, documented loss on an edge that leans on route ordering. We accept it (re-append) rather than attempt a stale-index `PUT .../routes/<origIndex>` insert; the undo's success message does not claim positional fidelity. Test it explicitly.

**Arming point** (r1-F4): arm `m.lastPurge` **only for an OWNED purge** (OQ8 — a foreign force-purge is one-way: no arm, no capture stored, no restore offered), and **after the takeover `publishDoneMsg`** (arm even if the takeover *fails* — the purge succeeded, so restore must be offered), and **exempt the in-transaction takeover publish from the "new publish clears the banner" trigger** — otherwise the takeover wipes the affordance microseconds after arming it.

**Binding + clear-triggers.** Restore binds to a **key shown in the action line**. Rev-1 leaned to contextual `u`; round 1 (F6) showed that overloads the registry-undo key (a "shadow window" that silently disables documented `u`, and a `u,u` chain that restore-then-registry-undoes) → **rev-2 recommends a distinct key**, keeping `u` purely registry (still add a one-line pointer in the `u` help). Clears on: successful restore; a *new* (non-takeover) publish/purge; a poll/(C)-read showing third-party re-take; **de-escalation of our takeover** (r1-F10, new); navigation; or a ~60s timeout (**OQ1**).

## 4. Component changes

**`internal/caddyedge`** (new primitives; no change to existing exported behavior):
- `InspectConflict` (two reads — `@id` + raw overlap scan; `Kind` incl. `IdHijacked`), `PurgeConflict` (re-verify `expect` incl. owned backend; delete by `@id`/index under `If-Match`; `ErrNoConflict`/`ErrConflictChanged`), `RestoreRoute(json.RawMessage)` (POST-append raw; never re-serializes). A raw-fetch helper (`[]json.RawMessage` + raw `GET /id`). `Unpublish` reused verbatim for undo-A.

**`internal/ui`:**
- New confirm entry-modes (`entryConfirmPurgeOwned` y/n; `entryConfirmPurgeForeign` y/n + `entryConfirmPurgeForeignType` typed). Messages/commands: `inspectConflictCmd/Msg`, `purgeCmd/purgeDoneMsg`, `restoreCmd/restoreDoneMsg`. Branch on the `publishDoneMsg` `ErrHostnameConflict` path (force-purge enabled).
- `pendingPublish` carry (§3.0). Sibling poof ticker + **one bottom-region action line** through the status slot (§3.5) — **not** a third sticky banner. `m.lastPurge` slot + in-flight `restoring` guard. `u` help gains a pointer to the (separately-keyed) restore affordance.

## 5. Back-compat / interaction

- **w131 domain flow / guards:** untouched; the conflict branch is strictly downstream (verified).
- **15s poll:** gen-stamped, owned-this-machine-filtered; a poll landing mid-poof only rewrites the list; it clears `m.lastPurge` only on observing a third-party re-take.
- **Local `undoStack`:** completely separate (never `stepHistory`/`saveConfig`/`wouldUnlockSSH`) — keeps the `u` promise literally true.
- **"funnelled AND published" drift:** a purge touches only a remote hostname's route, never local funnel state — can't create/collapse the marker.
- **Serve side-effect (r1-F12, new OQ-adjacent):** `publishCmd` enables serve *before* Publish (ui.go:1516), so by confirm-time serve is already on. On the **failure** path today a toast fires; the new **cancel** path is silent — a user who backs out of a scary foreign confirm reasonably thinks nothing changed, but serve is on. Benign (tailnet-only) but a footgun → emit a flash on cancel-after-serve-enable ("serve left on for :port — space to stop"). → candidate OQ.

## 6. Open Questions for mg

**The big one:**
- **OQ6 — Is refuse-with-attribution enough for v1?** Both reviewers converge YES-first. Pillar 1 (format the attribution Publish's error *already carries* — a **pure UI change, zero new `caddyedge` primitives**, review r2-m1) has none of the failure modes here. Force-purge + poof + undo (pillars 2–4) are the complex, remote-mutating, hard-to-verify layer with a blocker + 6 majors' worth of undo hazards now addressed but still large. **Recommend: ship pillar 1 now; land purge/poof/undo as a gated fast-follow** (mirrors ycv1's OQ deferrals). *If you agree, `qfbf` becomes the small pillar-1 UI change and the rest re-sequence behind it.*

**If we build the purge/undo layer:**
1. **OQ1 — undo lifetime:** ~60s + competing-action clears (draft), or until next competing action only (no timer), or app-exit?
2. **OQ2 — undo ordering:** confirm **A-before-B** (both reviewers endorse), accepting the blunt "now UNCLAIMED"/"another route claims it" messages computed from the final re-read.
3. **OQ3 — which key restores:** rev-2 recommends a **distinct key** shown in the action line (round 1 showed contextual `u` shadows registry-undo and enables a `u,u` footgun). Confirm.
4. **OQ4 — the scary keystroke:** type `purge` (draft) vs the hostname vs `force`; two gates (y/n → type) vs one typed gate.
5. ~~**OQ5 — id-less-foreign order fidelity**~~ — **DISSOLVED by OQ8**: undo is owned-only, an owned route always has an `@id`, so nothing is ever restored by array-index and there is no precedence loss.
7. **OQ7 — cross-machine owned takeover:** one normal confirm for all owned (draft), or a distinct/scarier confirm when the owned route belongs to *another* machine/user (detectable; review r2-m2 pushes here)?
8. **OQ8 — foreign-route undo vs the drift philosophy** *(the sharpest)*: undoing a *foreign* purge **re-creates drift** on the user's behalf. Offer undo only for **owned** purges (foreign purge = deliberate, one-way)? This also dissolves OQ5's worst case.
9. **OQ-Poof (new) — restore-affordance primitive:** one transient status-slot action line carrying poof→restore (rev-2), vs a distinct sticky banner, vs the poof slot? (Rev-2 picks the status-slot line to avoid a permanent reserved row.)
10. **OQ-Serve (new):** flash on cancel-after-serve-enable so a backed-out publish doesn't silently leave serve on?
11. **OQ-Version (from B1):** state a **Caddy ≥ 2.5.2** requirement in the docs (the whole concurrency model needs it; shipped `caddy:2-alpine` already satisfies it) — recommend, need your OK to document a hard floor.

## 7. Verification plan

**Unit-testable against the `httptest` fake — but the fake MUST be rebuilt first (review r1-F7):** the current fake stores `[]Route` (itself lossy — can't even *seed* a foreign `static_response` with fields tailport doesn't model), uses a single **global** etag string (no path scope), and blindly `append`s on POST (no `@id`-uniqueness). So today a "byte-faithful capture" test, a "parent-scope If-Match" test, and a "duplicate-`@id`-on-POST" test would all pass **for the wrong reason or be impossible**. Rebuild the fake to store **`[]json.RawMessage`**, model **path-scoped etags**, and reject **duplicate `@id` on POST**, then test: `InspectConflict` classification (incl. non-proxy foreign *found*, and the `IdHijacked` case); `PurgeConflict` `@id`-vs-index selection, `expect` re-verify → `ErrConflictChanged`, 412-retry; **byte-faithful** capture→restore of an OWNED route that carries a foreign-added field tailport doesn't model (exact bytes — proves raw capture is robust even for an owned route; we never restore a foreign route, per OQ8); **B refuses-before-append** when the name is re-claimed (an overlapping foreign route or a duplicate `@id`) rather than creating dual exposure; undo message selection driven by B's scan + the (C) **full-array** re-read across unclaimed/claimed-by-us/claimed-by-third/**overlap** — including a **concurrent overlapping route appended between B and C** (the C-scan must catch it and NOT say "restored", roborev-k7br-#2); the **accepted precedence loss** (a restored owned route re-appends at the array end, so its order relative to a seeded catch-all changes — assert the re-append and that the success message makes no positional claim, roborev-k7br-#1); in-flight restore guard; the confirm ladders; the action-line/status-slot reservation.

**Honest CI-vs-live line:** the Caddy *mechanics* are now **source-verified** (§3.2) — CI needn't pretend to prove them, but tailport's **own** code has never exercised a **parent-scope `If-Match`** or **index-DELETE** against a real Caddy, so extend the opt-in `pxrx` integration test to cover an id-less-foreign index-delete-under-parent-ETag, and prove the version-floor behavior. Live-9kgt end-to-end: two machines racing a takeover; force-purge of a genuinely hand-authored foreign route; undo across a real edge; a real duplicate-`@id`-on-POST rejection. **Do not claim the fake proves the mechanics.**

## 8. Review log

- **Round 1 (opus — remote-undo attack):** 1 blocker + 6 major, all rooted in one structural error (a fixed message-table where the post-undo state is a product of outcomes) → rev-2 recomputes the undo message from a **mandatory final re-read**; splits `Unpublish` `ErrNotFound` vs `ErrHostnameConflict` (F1); fixes the arming/clear collision (F4), the missing in-flight guard (F5), the `u`-overload shadow window (F6 → distinct key), the retryable-mode re-arm/timer race (F11); adds `expect`-pins-backend (F8), `Index`/etag-are-display-only (F9), de-escalation-clears (F10), cancel-leaves-serve OQ (F12), re-entrant double-purge OQ (F14). Confirmed sound: raw-bytes capture, A-before-B, `expect` re-verify, no-plaintext carry.
- **Round 2 (opus — Caddy admin-API + UI, web-verified):** **promoted** both admin-API mechanics to **source-verified** (index-DELETE deletes-and-shifts, `admin.go`; parent-scope `If-Match` re-hashes at the embedded path, PR #4579). New **blocker B1**: Caddy **version floor ≥ 2.5.2** (below it `If-Match` is ignored, all 412-retry is dead, silent clobber). **M1**: `InspectConflict` must do a second `@id` read to classify the hijacked-`@id` case (else livelock). **M2/M3**: the poof + restore must not be a third sticky banner + an unreserved line → one status-slot action line. Confirmed sound: capture, A-before-B, sibling poof ticker, off-registry-stack undo, guard ordering, `m.published` filter.
- **Convergence:** both rounds independently recommend **OQ6 (ship refuse-with-attribution first)**; pillar 1 needs zero new primitives (r2-m1) and carries none of the undo hazards.

**Sources (Caddy admin-API):** [caddyserver.com/docs/api](https://caddyserver.com/docs/api) · [caddyserver/caddy#4579 (ETag/If-Match)](https://github.com/caddyserver/caddy/pull/4579) · Caddy `admin.go` `unsyncedConfigAccess` (index-DELETE) · Caddy v2.5.2 release (ETag/If-Match introduced).
