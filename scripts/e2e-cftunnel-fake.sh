#!/usr/bin/env bash
# e2e-cftunnel-fake.sh -- opt-in, NOT run in CI (kata nc1j, W4, design-v033-
# final.md; rewritten for kata p7c5's o/O split). Drives a REAL compiled
# tailport binary inside tmux through the full Cloudflare Tunnel flow --
# both the quick tunnel (`o`) and the config.yaml-bound named tunnel (`O`) --
# against the Tier 2 fake cloudflared (internal/cftunnel/fakecloudflared) --
# no Cloudflare account, no network beyond loopback, and NEVER the real
# cloudflared binary (that would open a real trycloudflare/named tunnel --
# see the coordinator's amendment A4).
#
# Usage: scripts/e2e-cftunnel-fake.sh
#
# It builds tailport and the fake fresh, runs the step flow below, and
# prints one PASS/FAIL line per step plus a final summary. Every step is
# expected to PASS: the named/quick lifecycle, the same-tunnel guard, and
# exit-toast detection this script exercises are all already-shipped
# functionality (S3/S4/S5, kata nc1j) -- p7c5 only changed how `o`/`O` REACH
# that functionality (straight to a confirm, no mode-select or text prompts,
# `O` driven by config.yaml), not the functionality itself.
#
# Isolation (never touches the invoking user's real tailport state):
#   - a mktemp scratch dir holds a scratch HOME (with a non-empty, otherwise
#     meaningless ~/.cloudflared/cert.pem -- the fake only checks it exists),
#     XDG_CONFIG_HOME, XDG_STATE_HOME, XDG_DATA_HOME, and a scratch config.yaml;
#   - PATH is the fake's own directory FIRST, so cftunnel.Client.Detect()/
#     LoggedIn()/binary() all resolve to the fake, never a real cloudflared
#     that might happen to be installed (see internal/cftunnel/cftunnel.go's
#     binary()/Detect()/LoggedIn(), and discover.go's isCloudflaredArgv0).
#
# Robustness:
#   - every wait is capture-pane polling with a bounded timeout, never a bare
#     sleep for anything that depends on the poll ticker or the fake's own
#     timers;
#   - cleanup NEVER uses `pkill -f`/`pgrep -f`: a -f pattern matches against
#     every process's FULL command line, including this very script's own
#     (e.g. a bash -c invocation that literally contains the pattern text),
#     and would kill the script's own shell out from under it. Every process
#     this script starts is tracked by PID ($! for what it launches directly,
#     or parsed out of a fake's own ".console" startup line for what
#     tailport launches on our behalf) and killed by that exact PID.
set -uo pipefail
# Deliberately NOT `set -e`: this script's JOB is to keep going through all
# 11 steps and report each one, not to abort at the first (possibly
# EXPECTED) failure.

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

for tool in tmux go python3 curl; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "FATAL: $tool is required to run this script" >&2
        exit 1
    fi
done

SESSION="cftunnel-e2e-$$"
SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/cftunnel-e2e.XXXXXX")

# ---------------------------------------------------------------------------
# Result bookkeeping
# ---------------------------------------------------------------------------
declare -a STEP_LOG=()
FAIL_COUNT=0

record() { # name result note
    local name="$1" result="$2" note="$3"
    STEP_LOG+=("$result|$name|$note")
    printf '[%s] %s\n      %s\n' "$result" "$name" "$note"
    if [ "$result" = FAIL ]; then FAIL_COUNT=$((FAIL_COUNT + 1)); fi
}

# ---------------------------------------------------------------------------
# Cleanup: ALWAYS kills the tmux session, every fake cloudflared this run
# started, and every http.server it started -- by tracked PID only.
# ---------------------------------------------------------------------------
declare -a HTTPD_PIDS=()
declare -a FAKE_PIDS=()

cleanup() {
    local rc=$?
    echo
    echo "== cleanup =="
    tmux kill-session -t "$SESSION" >/dev/null 2>&1 || true
    for pid in "${FAKE_PIDS[@]:-}" "${HTTPD_PIDS[@]:-}"; do
        [ -n "${pid:-}" ] || continue
        kill -TERM "$pid" >/dev/null 2>&1 || true
    done
    sleep 0.3
    for pid in "${FAKE_PIDS[@]:-}" "${HTTPD_PIDS[@]:-}"; do
        [ -n "${pid:-}" ] || continue
        kill -KILL "$pid" >/dev/null 2>&1 || true
    done
    rm -rf "$SCRATCH"
    exit "$rc"
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# tmux / pane helpers
# ---------------------------------------------------------------------------
pane() { tmux capture-pane -t "$SESSION" -p; }
send() { tmux send-keys -t "$SESSION" "$@"; }

# pane_has SUBSTR [SUBSTR...] -- true if some ONE line of the current pane
# contains every given substring (plain substring match, not regex, so
# multi-byte glyphs like ◈/▲ and locale settings can't cause false
# negatives).
pane_has() {
    local line
    while IFS= read -r line; do
        local all=1 s
        for s in "$@"; do
            case "$line" in *"$s"*) ;; *) all=0; break ;; esac
        done
        if [ "$all" = 1 ]; then return 0; fi
    done <<<"$(pane)"
    return 1
}

# wait_until TIMEOUT_SECS SUBSTR [SUBSTR...] -- polls pane_has until it's true
# or the timeout elapses. Never a bare sleep for something timing-dependent.
wait_until() {
    local timeout="$1"; shift
    local deadline=$((SECONDS + timeout))
    while (( SECONDS < deadline )); do
        if pane_has "$@"; then return 0; fi
        sleep 0.2
    done
    return 1
}

# port_block_has PORT SUBSTR [SUBSTR...] -- like pane_has, but scoped to just
# the one service's block of lines (from its ":PORT" header down to the next
# blank line), rather than the whole pane. Needed from step 10 onward, where
# MULTIPLE ports can carry a live "cloudflare ... app.example.test" route row
# at once: a plain pane-wide pane_has "cloudflare" "app.example.test" would
# false-positive on an ALREADY-tunnelled port that merely happens to still be
# visible in view (tailport's fuzzy filter can keep other rows visible too --
# e.g. filtering "8793" fuzzy-matches ":8791 python3" too, since "python3"
# ends in a 3). Scoping to the port's own block is what makes the check mean
# what it says.
port_block_has() {
    local port="$1"; shift
    local block
    block=$(pane | awk -v p=":$port" '$0 ~ p && !found {found=1} found {print; if ($0 == "") exit}')
    local line
    while IFS= read -r line; do
        local all=1 s
        for s in "$@"; do
            case "$line" in *"$s"*) ;; *) all=0; break ;; esac
        done
        if [ "$all" = 1 ]; then return 0; fi
    done <<<"$block"
    return 1
}

# wait_until_port TIMEOUT_SECS PORT SUBSTR [SUBSTR...] -- port_block_has,
# polled with a bounded timeout.
wait_until_port() {
    local timeout="$1" port="$2"; shift 2
    local deadline=$((SECONDS + timeout))
    while (( SECONDS < deadline )); do
        if port_block_has "$port" "$@"; then return 0; fi
        sleep 0.2
    done
    return 1
}

# wait_absent TIMEOUT_SECS SUBSTR -- polls until SUBSTR is NOT present on any
# line, or the timeout elapses.
wait_absent() {
    local timeout="$1" substr="$2"
    local deadline=$((SECONDS + timeout))
    while (( SECONDS < deadline )); do
        if ! pane_has "$substr"; then return 0; fi
        sleep 0.2
    done
    return 1
}

# start_httpd PORT -- starts a plain python3 http.server bound to loopback,
# tracked by exact PID (exec replaces the subshell, so $! IS python3's own
# pid -- no wrapper shell to lose track of, and nothing for a pkill/pgrep -f
# to have to guess at).
start_httpd() {
    local port="$1"
    ( cd "$SCRATCH/www" && exec python3 -m http.server "$port" --bind 127.0.0.1 \
        >"$SCRATCH/httpd-$port.log" 2>&1 ) &
    disown
    HTTPD_PIDS+=("$!")
    local deadline=$((SECONDS + 5))
    while (( SECONDS < deadline )); do
        if curl -s -o /dev/null "http://127.0.0.1:$port/"; then return 0; fi
        sleep 0.1
    done
    echo "FATAL: python http.server on :$port never came up" >&2
    exit 1
}

# fake_pid_of CONSOLE_PATH -- extracts the fake's own pid from the startup
# line it writes to its console capture ("... pid=NNNN"), so cleanup and the
# quit/relaunch assertions can track it by exact PID without ever pattern-
# matching a live process's command line.
fake_pid_of() {
    grep -oE 'pid=[0-9]+' "$1" 2>/dev/null | tail -1 | cut -d= -f2
}

# track_fake CONSOLE_PATH -- looks up a just-started fake's pid (see
# fake_pid_of) and adds it to FAKE_PIDS so cleanup's trap kills it
# unconditionally, regardless of whether the step that started it went on to
# PASS or FAIL. Every tunnel this script starts anywhere below is tracked
# this way the moment it starts, not only the ones an assertion happens to
# check the pid of later.
track_fake() {
    local pid
    pid=$(fake_pid_of "$1")
    [ -n "$pid" ] && FAKE_PIDS+=("$pid")
}

# filter_to PORT -- opens the "/" filter, clears any previous query, types
# the port number, and commits with enter. FilterValue includes the port
# number (ui.go's portItem.FilterValue, e518), so this reliably narrows the
# list to exactly one service row regardless of what else is listening on
# this machine.
filter_to() {
    local port="$1"
    send "/"
    sleep 0.1
    send "C-u" # bubbles/textinput's default "clear to start of line"
    sleep 0.1
    send "$port"
    sleep 0.2
    send "Enter"
    sleep 0.2
}

# ---------------------------------------------------------------------------
# Build tailport and the fake, fresh, from this worktree.
# ---------------------------------------------------------------------------
mkdir -p "$SCRATCH/bin" "$SCRATCH/fakebin" "$SCRATCH/www" \
    "$SCRATCH/home/.cloudflared" "$SCRATCH/xdgcfg/tailport" "$SCRATCH/xdgstate" "$SCRATCH/xdgdata"

echo "building the fake cloudflared..."
if ! ( cd "$REPO_ROOT" && go build -o "$SCRATCH/fakebin/cloudflared" ./internal/cftunnel/fakecloudflared ); then
    echo "FATAL: building the fake cloudflared failed" >&2
    exit 1
fi

# E2E_TAILPORT_BIN lets this script be pointed at an ALREADY-built tailport
# binary instead of building one from this worktree -- e.g. to cross-check
# this same script/fake against a tailport built from a different branch
# (read-only: this script only ever RUNS that binary, never touches the
# worktree it came from). Default: build fresh from this worktree, same as
# any other run.
if [ -n "${E2E_TAILPORT_BIN:-}" ]; then
    TAILPORT_BIN="$E2E_TAILPORT_BIN"
    echo "using externally supplied tailport binary: $TAILPORT_BIN (skipping build)"
else
    TAILPORT_BIN="$SCRATCH/bin/tailport"
    echo "building tailport..."
    if ! ( cd "$REPO_ROOT" && go build -o "$TAILPORT_BIN" ./cmd/tailport ); then
        echo "FATAL: building tailport failed" >&2
        exit 1
    fi
fi

# A non-empty cert.pem: the fake never validates it, and LoggedIn() only
# checks the file EXISTS -- see cftunnel.go's LoggedIn doc comment.
echo "e2e-fake-cert, not a real credential" >"$SCRATCH/home/.cloudflared/cert.pem"

# The named tunnel is bound PER PORT in config.yaml (kata p7c5) -- no more
# cloudflared.domain prefill, no more mode-select/text prompts. :8791 and
# :8793 are bound to the SAME tunnel name ("web") but different hostnames,
# so :8793 can exercise the same-tunnel guard (step 9 below) against
# whichever port is actually running "web"; :8792 is deliberately left
# UNBOUND to exercise the "no binding" refusal (step 1).
cat >"$SCRATCH/xdgcfg/tailport/config.yaml" <<'EOF'
ports:
  8791:
    cloudflare:
      tunnel: web
      hostname: app.example.test
  8793:
    cloudflare:
      tunnel: web
      hostname: app2.example.test
EOF

ARGV_LOG="$SCRATCH/argv.log"
: >"$ARGV_LOG"

# ---------------------------------------------------------------------------
# tmux session: a plain persistent shell, NOT `tailport` as the pane's
# command directly -- so that quitting tailport leaves the pane (and the
# tmux server) alive to relaunch it from, rather than tmux tearing the whole
# session down the moment its one window's process exits.
# ---------------------------------------------------------------------------
tmux new-session -d -s "$SESSION" -x 220 -y 50 bash --norc
sleep 0.3
send "export HOME=$SCRATCH/home XDG_CONFIG_HOME=$SCRATCH/xdgcfg XDG_STATE_HOME=$SCRATCH/xdgstate XDG_DATA_HOME=$SCRATCH/xdgdata PATH=$SCRATCH/fakebin:\$PATH FAKE_CF_ARGV_LOG=$ARGV_LOG FAKE_CF_MAX_LIFETIME=10m" Enter
sleep 0.3

launch_tailport() {
    send "$TAILPORT_BIN" Enter
    sleep 1
}

argv_order_ok() {
    python3 - "$ARGV_LOG" <<'PYEOF'
import json, sys
ok = False
with open(sys.argv[1]) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            args = json.loads(line)
        except Exception:
            continue
        if "run" not in args:
            continue
        run_idx = args.index("run")
        flags = ("--config", "--metrics", "--logfile", "--no-autoupdate")
        if not all(f in args for f in flags):
            continue
        if not all(args.index(f) < run_idx for f in flags):
            continue
        if "--url" not in args or args.index("--url") < run_idx:
            continue
        if args[-1] != "web":
            continue
        ok = True
sys.exit(0 if ok else 1)
PYEOF
}

echo
echo "== starting the flow =="
echo

start_httpd 8791
start_httpd 8792
start_httpd 8793
start_httpd 8794
launch_tailport
send "a" # all ports (fresh ad-hoc http.servers aren't tracked favorites)
sleep 0.3

# ---------------------------------------------------------------------------
# Step 1: O on an UNBOUND port -> toast naming the fix, no modal opened.
# ---------------------------------------------------------------------------
filter_to 8792
if ! wait_until 5 ":8792"; then
    echo "FATAL: :8792 never appeared in the port list" >&2
    exit 1
fi
send "O"
if wait_until 3 "has no named tunnel" "ports.8792.cloudflare"; then
    record "1. O on unbound port -> toast" PASS "refused naming the fix: $(pane | grep -F 'has no named tunnel' | tail -1)"
else
    record "1. O on unbound port -> toast" FAIL "no refusal toast: $(pane | tail -3)"
fi

# ---------------------------------------------------------------------------
# Step 2: O on the BOUND port :8791 -> confirm names host + tunnel straight
# from config.yaml. No mode select, no hostname/name prompt -- this IS the
# whole setup.
# ---------------------------------------------------------------------------
filter_to 8791
if ! wait_until 5 ":8791"; then
    echo "FATAL: :8791 never appeared in the port list" >&2
    exit 1
fi
send "O"
confirm_url_ok=1
wait_until 3 "https://app.example.test" || confirm_url_ok=0
confirm_name_ok=1
pane_has 'via tunnel "web" (from config.yaml)' || confirm_name_ok=0
if [ "$confirm_url_ok" = 1 ] && [ "$confirm_name_ok" = 1 ]; then
    record "2. O on bound port -> confirm from config" PASS "confirm shows https://app.example.test AND via tunnel \"web\" (from config.yaml)"
else
    record "2. O on bound port -> confirm from config" FAIL "confirm_url_ok=$confirm_url_ok confirm_name_ok=$confirm_name_ok: $(pane | tail -5)"
fi

CONSOLE1="$SCRATCH/xdgstate/tailport/cftunnel-8791-app.example.test.console"

send "y"
sleep 0.5
track_fake "$CONSOLE1"

# ---------------------------------------------------------------------------
# Step 3: argv log has the exact shape (tunnel-level flags before run,
# --url after, name last).
# ---------------------------------------------------------------------------
if argv_order_ok; then
    record "3. argv log has the exact order" PASS "--config/tunnel-level flags before run, --url after run, name last (see $ARGV_LOG)"
else
    record "3. argv log has the exact order" FAIL "no logged invocation matched the expected shape: $(tail -3 "$ARGV_LOG")"
fi

# ---------------------------------------------------------------------------
# Step 4: marker + URL appear, not stale, within a few seconds.
# ---------------------------------------------------------------------------
if wait_until 8 "◈" "app.example.test"; then
    record "4. marker+URL appear, not stale within 5s" PASS "◈ https://app.example.test shown, no ▲/stale"
else
    record "4. marker+URL appear, not stale within 5s" FAIL "never reached a non-stale ◈ row: $(pane | sed -n '1,8p')"
fi

# ---------------------------------------------------------------------------
# Step 5: quit tailport; the fake (detached, its own session) must survive.
# ---------------------------------------------------------------------------
send "q"
sleep 0.5
PID1=$(fake_pid_of "$CONSOLE1")
if [ -n "$PID1" ] && kill -0 "$PID1" 2>/dev/null; then
    record "5. fake survives tailport quit" PASS "fake cloudflared pid $PID1 still running after quit"
else
    record "5. fake survives tailport quit" FAIL "pid not found (or not alive) from $CONSOLE1"
fi

# ---------------------------------------------------------------------------
# Step 6: relaunch; the tunnel is rediscovered with its hostname, no re-confirm.
# ---------------------------------------------------------------------------
launch_tailport
send "a"
sleep 0.3
filter_to 8791
if wait_until 6 "◈" "app.example.test"; then
    record "6. relaunch rediscovers tunnel+hostname" PASS "rediscovered immediately with https://app.example.test, no re-confirm"
else
    record "6. relaunch rediscovers tunnel+hostname" FAIL "tunnel not rediscovered after relaunch: $(pane | sed -n '1,8p')"
fi

# ---------------------------------------------------------------------------
# Step 7: O tears it down (de-escalation, no confirm).
# ---------------------------------------------------------------------------
send "O"
sleep 0.3
torn_down=1
wait_absent 3 "◈" || torn_down=0
sleep 0.3
proc_gone=1
kill -0 "$PID1" 2>/dev/null && proc_gone=0
if [ "$torn_down" = 1 ] && [ "$proc_gone" = 1 ]; then
    record "7. O tears it down" PASS "row cleared and pid $PID1 exited, no confirm prompt"
else
    record "7. O tears it down" FAIL "torn_down=$torn_down proc_gone=$proc_gone"
fi

# ---------------------------------------------------------------------------
# Step 8: O again -- config.yaml IS the memory now (kata p7c5: no session
# re-raise), so the confirm shows the SAME host+tunnel as step 2, freshly
# re-derived, and confirming it restarts successfully.
# ---------------------------------------------------------------------------
send "O"
sleep 0.3
reraise_url_ok=1
pane_has "https://app.example.test" || reraise_url_ok=0
reraise_name_ok=1
pane_has 'via tunnel "web" (from config.yaml)' || reraise_name_ok=0
send "y"
sleep 0.5
track_fake "$CONSOLE1" # picks up the NEW pid from this restart
PID1=$(fake_pid_of "$CONSOLE1")
restarted_ok=1
wait_until 8 "◈" "app.example.test" || restarted_ok=0
if [ "$reraise_url_ok" = 1 ] && [ "$reraise_name_ok" = 1 ] && [ "$restarted_ok" = 1 ]; then
    record "8. O again -- confirm again from config.yaml" PASS "same confirm shown (no memory needed) and tunnel restarted successfully"
else
    record "8. O again -- confirm again from config.yaml" FAIL "reraise_url_ok=$reraise_url_ok reraise_name_ok=$reraise_name_ok restarted_ok=$restarted_ok: $(pane | tail -5)"
fi

# ---------------------------------------------------------------------------
# Step 9: same-tunnel refusal. :8793 is bound to the SAME tunnel name
# ("web") as :8791, which is running it (from step 8) -- O on :8793 must
# refuse, naming :8791 and pointing at O (not o) to free it.
# ---------------------------------------------------------------------------
filter_to 8793
if ! wait_until 5 ":8793"; then
    echo "FATAL: :8793 never appeared in the port list" >&2
    exit 1
fi
send "O"
if wait_until 3 'already running for :8791' 'press O on :8791'; then
    record "9. same-tunnel refusal on second port" PASS "refused: $(pane | grep -F 'already running for :8791' | tail -1)"
else
    record "9. same-tunnel refusal on second port" FAIL "no refusal toast: $(pane | tail -3)"
fi

# ---------------------------------------------------------------------------
# Step 10: cross-key refusal #1 -- o on :8791, which is running its NAMED
# tunnel, must refuse and point at O (never tear down or touch the wrong mode).
# ---------------------------------------------------------------------------
filter_to 8791
if ! wait_until 5 ":8791"; then
    echo "FATAL: :8791 never appeared in the port list" >&2
    exit 1
fi
send "o"
if wait_until 3 "a named tunnel is running on :8791" "press O to stop it"; then
    record "10. o refuses a running named tunnel" PASS "refused, exact wording: $(pane | grep -F 'a named tunnel is running' | tail -1)"
else
    record "10. o refuses a running named tunnel" FAIL "no refusal toast: $(pane | tail -3)"
fi
# :8791's named tunnel must still be running -- o's cross-key refusal must
# not have torn it down.
if ! pane_has "◈" "app.example.test"; then
    record "10b. named tunnel on :8791 still running after o's refusal" FAIL "◈ app.example.test row is gone: $(pane | sed -n '1,8p')"
else
    record "10b. named tunnel on :8791 still running after o's refusal" PASS "row still shows ◈ https://app.example.test"
fi

# ---------------------------------------------------------------------------
# Step 11: o on a fresh port -> straight to the quick confirm. No mode
# select, no hostname/name prompt -- this is the whole setup for a quick
# tunnel, regardless of login state (a cert.pem is present in this scratch
# HOME, and o still never offers named).
# ---------------------------------------------------------------------------
filter_to 8794
if ! wait_until 5 ":8794"; then
    echo "FATAL: :8794 never appeared in the port list" >&2
    exit 1
fi
send "o"
if wait_until 3 "Cloudflare quick tunnel"; then
    record "11. o goes straight to the quick confirm" PASS "quick confirm shown, no mode select or prompt"
else
    record "11. o goes straight to the quick confirm" FAIL "quick confirm never appeared: $(pane | tail -5)"
fi
send "y"
sleep 0.5
CONSOLE_QUICK="$SCRATCH/xdgstate/tailport/cftunnel-8794.console"
track_fake "$CONSOLE_QUICK"
if wait_until 8 "◈" "fake-8794.trycloudflare.com"; then
    record "11b. quick tunnel comes up on :8794" PASS "◈ fake-8794.trycloudflare.com shown"
else
    record "11b. quick tunnel comes up on :8794" FAIL "quick tunnel never came up: $(pane | sed -n '1,8p')"
fi

# ---------------------------------------------------------------------------
# Step 12: cross-key refusal #2 -- O on :8794, which is running a QUICK
# tunnel, must refuse and point at o.
# ---------------------------------------------------------------------------
send "O"
if wait_until 3 "a quick tunnel is running on :8794" "press o to stop it"; then
    record "12. O refuses a running quick tunnel" PASS "refused, exact wording: $(pane | grep -F 'a quick tunnel is running' | tail -1)"
else
    record "12. O refuses a running quick tunnel" FAIL "no refusal toast: $(pane | tail -3)"
fi
if ! pane_has "◈" "fake-8794.trycloudflare.com"; then
    record "12b. quick tunnel on :8794 still running after O's refusal" FAIL "◈ fake-8794.trycloudflare.com row is gone: $(pane | sed -n '1,8p')"
else
    record "12b. quick tunnel on :8794 still running after O's refusal" PASS "row still shows ◈ fake-8794.trycloudflare.com"
fi

# ---------------------------------------------------------------------------
# Step 13: FAKE_CF_FAIL_AFTER produces an exit toast (vanish detection).
# Needs a fresh relaunch (the fake reads the env var from the PROCESS that
# spawns it -- tailport -- at spawn time, so it must be exported in the
# pane's shell BEFORE tailport starts, not after) and a fresh port, so it
# can't be confused with :8794's already-healthy quick tunnel above.
# ---------------------------------------------------------------------------
send "q"
sleep 0.5
send "export FAKE_CF_FAIL_AFTER=3s:e2e-induced-crash" Enter
sleep 0.2
start_httpd 8795
launch_tailport
send "a"
sleep 0.3
filter_to 8795
if ! wait_until 5 ":8795"; then
    echo "FATAL: :8795 never appeared in the port list" >&2
    exit 1
fi
send "o"
sleep 0.2
send "y"
sleep 0.5
track_fake "$SCRATCH/xdgstate/tailport/cftunnel-8795.console"

quick_up=1
wait_until 10 "◈" "fake-8795.trycloudflare.com" || quick_up=0
# Let FAKE_CF_FAIL_AFTER=3s actually fire, then give the poll ticker
# (tunnelPollInterval=4s) a full cycle plus margin to notice.
sleep 8
result13=$(pane)
if [ "$quick_up" != 1 ]; then
    record "13. exit toast on vanished tunnel" FAIL "setup precondition failed: quick tunnel on :8795 never came up"
elif echo "$result13" | grep -qF "Cloudflare tunnel on :8795" && echo "$result13" | grep -qi "exited"; then
    record "13. exit toast on vanished tunnel" PASS "exit toast shown: $(echo "$result13" | grep -i "exited" | tail -1)"
else
    record "13. exit toast on vanished tunnel" FAIL "no toast mentioning the exit ever appeared: $(pane | tail -5)"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo
echo "== summary =="
for entry in "${STEP_LOG[@]}"; do
    IFS='|' read -r result name _ <<<"$entry"
    printf '  [%s] %s\n' "$result" "$name"
done
echo
echo "$FAIL_COUNT step(s) failed."
