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

- Tailnet-only. `tailscale funnel` (public internet exposure) is never invoked
  by this tool, under any flag or code path. `tailscale serve` only.
- Plain HTTP only (`--http=PORT`). No HTTPS/TLS serve mode — deliberate,
  see project history: Tailscale's WireGuard tunnel already encrypts
  peer-to-peer traffic, so app-layer TLS added no real confidentiality
  here, and it would have pulled in cert/HTTPS complexity for no benefit.
- 1:1 port mapping only — the exposed tailnet port always equals the local
  port. No remapping (public port != local port).
- Fleet targets: `linux/amd64` (host-a, host-b) and `darwin/arm64` (mac-a,
  mac-b, mac-c). Keep `.github/workflows/build.yml` and `install.sh` in
  sync with this list if it changes.
- Zero non-Go runtime dependencies in the shipped binary. It shells out
  to `tailscale`, and to `ss` (Linux) / `lsof` (macOS) for port discovery
  — nothing else. Don't add a dependency on `yq`, `gum`, `fzf`, etc.;
  config parsing uses `gopkg.in/yaml.v3` natively for this reason.

### Verification bar

- A kata issue does not close on "it compiles." Run `go build ./...`,
  `go vet ./...`, and `go test ./...` at minimum. Where the change is
  user-visible (TUI behavior, a CLI flag, an actual `tailscale serve`
  interaction), exercise it for real — e.g. a detached `tmux` session
  driving the compiled binary with `send-keys`/`capture-pane`, or a
  throwaway `go run` harness against live `tailscaled` — and cite what
  you actually observed in the close message, not just that it built.
- If something can't be verified in the current environment (e.g. the
  macOS `lsof` path with no Mac available, or a GitHub Actions run with
  no pushed remote), say so explicitly. Either leave the issue open with
  `needs-review` and a comment describing exactly what's blocked, or close
  it with an honest caveat in the message — never claim untested code
  paths as verified.

### Workflow

- Use kata for all real feature/bug work in this repo. Search before
  creating, claim before starting, close only with evidence.
- Parallel feature work happens in git worktrees, one subagent per
  feature branch, rebased (not merge-commit) into `main` once its kata
  issue is closed with verification.
- No force-push, no `git reset --hard`, no skipping hooks, without
  explicit user authorization for that specific action.
