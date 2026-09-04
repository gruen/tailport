<!-- BEGIN KATA (managed by `kata init --with-agents`) -->
## kata issue tracker

This project uses [kata](https://github.com/kenn-io/kata) as its shared issue
ledger. Run `kata quickstart` at the start of each session for the full agent
contract. The short version:

- Search before creating: `kata search "<keywords>" --agent`.
- Prefer updating existing issues over duplicates (`kata comment`, `kata label add`, `kata edit`).
- Default to `--agent` for ordinary reads and mutations; use `--json` only when a script needs structured data.
- Close only verified work: `kata close <ref> --done --message "<scope + verification>" --commit <sha>`.
- If work is incomplete, label `needs-review` and comment what remains rather than closing.
- Never `kata delete` or `kata purge` without explicit user authorization.
<!-- END KATA -->

## Project rules

### Design constraints (do not relax without asking)

- Tailnet-first. `tailscale serve` (tailnet-only exposure) is the default
  path. `tailscale funnel` (public internet exposure) IS supported, but only
  as a deliberate, per-service opt-in via the `P` key (swapped from `p` under
  kata vzj4 — capital guards the more-permanent exposure) behind a strong y/n
  confirm that names the port and shows the resulting public URL. `:22` (SSH)
  is hard-blocked from funnel. Never funnel implicitly, in bulk, or without
  that confirm. (Implemented under kata yt69: the `P` key, `entryConfirmFunnel`
  gate, and `tsserve.FunnelOn/FunnelOff/FunnelStatus`.)
- Publish-via-edge is a SECOND public path (the `p` key, kata v1z5; swapped
  from `P` under vzj4), **independent of and mutually exclusive with Funnel —
  not layered or ranked above it**. A local port can carry funnel OR publish,
  never both: the `p` path refuses a funnelled port and the `P` path refuses a
  Caddy-published port, each instructing the user to remove the other
  exposure first. There is
  no implicit precedence between them — dual exposure created outside tailport
  (a foreign Funnel or a manual Caddy edit) is surfaced as explicit drift (the
  warning affordance + a "funnelled AND published" description), never silently
  collapsed to one marker. It carries the same funnel-grade guardrails:
  per-service opt-in, a strong y/n confirm naming the exact `https://<hostname>`
  URL, `:22` hard-blocked, ungated de-escalation (an immediate unpublish, no
  confirm), and unpublish never touches serve state. The `p` key is a TOGGLE
  (kata prp1): pressed again on a port published earlier THIS session
  (remembered hostname + auth, session-only, never persisted), it re-publishes
  with that remembered config, skipping the host/auth setup prompts — still
  behind the same y/n confirm naming the exact hostname, UNLESS the owner has
  opted into `caddy.silent_republish` (config, default OFF), an
  owner-approved, documented exception to the always-confirm rule that skips
  even that confirm. This exception is scoped strictly to RE-publishing an
  ALREADY-consented hostname within the SAME session — a port's FIRST publish
  always confirms regardless of this setting, and Funnel's confirm is
  completely unaffected. A dedicated `e` key opens the same setup flow (also
  always confirming) to edit a port's publish config: it changes the AUTH of an
  already-published port in place, but REFUSES to move a still-published port to
  a new hostname (that would be a non-atomic delete-and-create that could leave
  the old route dangling — the user unpublishes first, kata sw2y); it never
  de-escalates. That refuse is BEST-EFFORT: it keys off the poll cache, which can
  be stale/empty, so a rename can still slip through in that narrow window (a
  pre-existing property of the cache-based model, not a regression; roborev
  44n7) — the authoritative fix (a live-route scan / atomic replace at publish
  time) is tracked in srx1 for v0.2.1. In this path **Tailscale
  supplies private tailnet transport only; Caddy owns the entire public trust
  plane** (custom-domain DNS, public `:443` ingress, TLS termination and
  renewal, hostname routing). Basic auth at the edge is a single SHARED
  credential stored as a bcrypt hash, never plaintext. Published state is read
  live from the edge's `@id`-tagged routes on a separate poll, never persisted
  per-port — Caddy is the source of truth, the same philosophy as serve/funnel
  state being read live. (Implemented under kata v1z5: `internal/caddyedge`,
  the `caddy:` config block, and the `p` key / `entryConfirmPublish` gate /
  published-state poll in `internal/ui`. The `p` toggle, the `e` edit key, the
  session-only `lastPublish` memory, and `caddy.silent_republish` were added
  under kata prp1.)
- Cloudflare Tunnel is a THIRD public path (the `t` key, kata nc1j),
  **mutually exclusive with BOTH Funnel and Publish — never layered or
  ranked above either**. A local port can carry at most one public
  exposure: `t` refuses an already-funnelled or already-published port, and
  `P`/`p` each refuse an already-tunnelled one, each instructing the user to
  remove the other exposure first. Exposure created outside tailport is
  surfaced as explicit drift (a "funnelled AND tunnelled — remove one"
  description, extending to "multiple public exposures — remove all but
  one" when all three collide), never silently collapsed to one marker.
  `:22` is hard-blocked, same as Funnel/Publish. Two flavours, matching
  Cloudflare's two account scenarios: a QUICK tunnel needs no account and
  gets a random, ephemeral `*.trycloudflare.com` hostname assigned only
  after cloudflared actually starts — so its confirm CANNOT name the exact
  public URL in advance, a DOCUMENTED, DELIBERATE deviation from the
  always-name-the-URL rule the other public paths follow, forced by
  cloudflared's own design (there is no way to reserve or predict the
  hostname before starting); a NAMED tunnel requires the operator to have
  already run `cloudflared tunnel login`, created the tunnel, and routed its
  hostname (`cloudflared tunnel route dns`) — a separate, one-time operator
  task, exactly like standing up the Caddy edge. tailport only ever RUNS a
  named tunnel (`cloudflared tunnel run --url http://localhost:PORT
  <name>`); it never mutates the user's Cloudflare account or DNS. Unlike
  Publish (a stateless client of a remote edge) or Funnel (a
  Tailscale-managed ingress slot), cloudflared is a LONG-RUNNING LOCAL
  PROCESS tailport supervises directly — and by design TUNNELS SURVIVE
  TAILPORT EXITING: cloudflared is spawned DETACHED (its own session), and
  the OS process table is the live source of truth, polled fresh every
  cycle and NEVER persisted, so a tunnel from a prior session is
  re-discovered and re-toggleable on the next launch. Only tailport-OWNED
  tunnels (carrying a sentinel `--logfile` flag tailport always passes) are
  tracked this way; a foreign `cloudflared` process is surfaced as drift,
  never signalled or touched. The whole feature is gated on `cloudflared`
  being installed — detected once at startup — so an absent binary drops
  the `t` key from the bar entirely: no key, no discovery, no polling.
  (Implemented under kata nc1j: `internal/cftunnel`, the `cloudflared:`
  config block, and the `t` key / `requestTunnel` gate / tunnel-state poll
  in `internal/ui`.)
- Serve (tailnet) is plain HTTP only (`--http=PORT`). No HTTPS/TLS serve
  mode — deliberate, see project history: Tailscale's WireGuard tunnel
  already encrypts peer-to-peer traffic, so app-layer TLS added no real
  confidentiality here, and it would have pulled in cert/HTTPS complexity
  for no benefit. Funnel is necessarily different: its public ingress is
  always HTTPS/TLS (Tailscale terminates TLS with the node's `ts.net` cert;
  there is no plain-HTTP funnel). The local proxy target stays plain
  `http://127.0.0.1:PORT` either way.
- 1:1 port mapping for serve (tailnet) — the exposed tailnet port always
  equals the local port; no remapping. Funnel is exempt because Tailscale
  restricts funnel ingress to ports `443`, `8443`, and `10000` only: the
  local target port is unrestricted, but the public port is one of those
  three (auto-assigned 443 → 8443 → 10000, max three concurrent funnels
  per node). Serve mappings stay strictly 1:1.
- Fleet targets: `linux/amd64` (host-a, host-b) and `darwin/arm64` (mac-a,
  mac-b, mac-c). Keep `.github/workflows/build.yml` and `install.sh` in
  sync with this list if it changes.
- Release-artifact targets (broader than the fleet): `build.yml` also
  builds `linux/arm64` purely so the AUR `tailport-bin` package can offer
  an `aarch64` binary (jtpx). It is a distribution artifact, not a deployed
  fleet node — don't add it to `install.sh`'s fleet list.
- Zero non-Go RUNTIME dependencies in the shipped binary. It shells out
  to `tailscale`, and to `ss` (Linux) / `lsof` (macOS) for port discovery
  — nothing else *required*. Don't add a dependency on `yq`, `gum`, `fzf`,
  etc.; config parsing uses `gopkg.in/yaml.v3` natively for this reason.
  This rule is about runtime/PATH binaries, NOT Go module dependencies:
  pure-Go module deps are fine (e.g. `golang.org/x/crypto/bcrypt`, added
  under kata v1z5 to hash the shared publish credential) — they compile into
  the single static binary and add nothing to what must be on the host's PATH.
  Carve-out (vnq7): an OPTIONAL, best-effort clipboard helper
  (`pbcopy` / `wl-copy` / `xclip` / `xsel`) may be shelled out to for the
  `c` copy-URL action, but it is never required to build or run — the
  primary clipboard path is OSC 52 (pure Go, no external binary), and a
  missing helper is silently skipped.
  Carve-out (nc1j): `cloudflared` is a SECOND optional, opt-in third-party
  binary, required only if you use the `t` Cloudflare Tunnel feature (see
  the Cloudflare Tunnel design-constraints bullet above and
  `internal/cftunnel`). Unlike the clipboard helper's fire-and-forget
  shell-out, cloudflared is a **supervised local daemon** — the first
  long-running process tailport itself spawns and supervises, distinct
  from the remote Caddy edge tailport only ever talks to over HTTP. Like
  the clipboard carve-out, it is never required to build or run tailport:
  absence just disables the `t` key (no key, no discovery, no polling) and
  costs nothing.

### Verification bar

- A kata issue does not close on "it compiles." Run `go build ./...`,
  `go vet ./...`, and `go test ./...` at minimum. Where the change is
  user-visible (TUI behavior, a CLI flag, an actual `tailscale serve`
  interaction), exercise it for real — e.g. a detached `tmux` session
  driving the compiled binary with `send-keys`/`capture-pane`, or a
  throwaway `go run` harness against live `tailscaled` — and cite what
  you actually observed in the close message, not just that it built.
- If something can't be verified in the current environment (e.g. a
  GitHub Actions run with no pushed remote, or an `aarch64` build with no
  ARM host), say so explicitly. The macOS `lsof` path
  (`internal/portscan`) can now be exercised on a native runner via the
  opt-in `[ci darwin]` job (`.github/workflows/darwin-tests.yml`), so
  "no Mac available" is no longer a blanket caveat — its parser is also
  unit-tested with fixtures. Either leave the issue open with
  `needs-review` and a comment describing exactly what's blocked, or close
  it with an honest caveat in the message — never claim untested code
  paths as verified.

### Workflow

- Use kata for all real feature/bug work in this repo. Search before
  creating, claim before starting, close only with evidence.
- File new work through the `/ticket` skill
  (`.claude/skills/ticket/SKILL.md`). It searches the ledger first, folds
  into an existing issue when one already owns the scope, pushes back when
  a request is unclear or collides with the design constraints above, and
  files at priority 2 by default (kata's scale is `0..4`, 0 = highest).
  It fires on `/ticket <request>` and also on its own when someone
  describes work they want tracked. It deliberately does not close,
  delete, or purge — filing work and finishing it are different jobs.
- Cut releases through the `/release` skill
  (`.claude/skills/release/SKILL.md`). It takes everything on main since
  the last tag, holds the release gate ("bump patch after all < p3 done",
  per j68f), writes the notes by hand, tags behind one explicit confirm,
  then verifies against the *published* artifact before closing the release
  ticket with that evidence. `RELEASING.md` stays the authoritative runbook
  for the mechanics. Packaging is not its job: on a tag, CI publishes the AUR
  packages (18cr) and the Homebrew formula (nqmn) and bot-commits both bumps
  back to `main`, so **main moves after a release** — expect two extra
  commits and don't mistake them for drift. Both publishers are scripts
  (`scripts/aur-publish.sh`, `scripts/brew-publish.sh`) with a `CHECK=1` mode
  that asserts re-running them for a published version is a no-op; that is how
  they're tested without cutting a throwaway release.
- `kata purge` is denied outright in `.claude/settings.json`: it is
  irreversible ("remove an issue + all its rows"), and no agent should
  reach for it autonomously. Purge by hand if you truly mean it.
  `kata delete` is left alone deliberately — it is a *soft* delete,
  reversible via `kata restore`, and already gated behind `--force` plus
  an exact `--confirm "DELETE <short_id>"` string.
- Signal that work has started by claiming the issue: `kata claim <ref>`
  (optionally with `--comment "<what I'm starting>"`). Ownership is the
  "actively being worked" signal — kata has no in-progress status, so an
  owned issue means someone is on it. Before claiming, check it isn't
  already owned (`kata show <ref>` / `kata list --unowned`) to avoid
  colliding with another agent; only `--force` a reclaim deliberately.
- Parallel feature work happens in git worktrees, one subagent per
  feature branch. Each subagent claims its kata issue before starting,
  then rebases (not merge-commit) into `main` once that issue is closed
  with verification.
- Match the subagent's model to the task. Default to **sonnet** for
  implementation work. Use **opus** for planning and design — the hard
  reasoning, subtle-correctness, and architectural-tradeoff work (e.g.
  the design constraints above). Use **haiku** for cheap, mechanical
  work (rote edits, formatting, boilerplate, log/output grep). Don't
  default everything to the top tier; reserve opus for a compelling
  reason.
- No force-push, no `git reset --hard`, no skipping hooks, without
  explicit user authorization for that specific action.
