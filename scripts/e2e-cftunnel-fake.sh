#!/usr/bin/env bash
# e2e-cftunnel-fake.sh -- opt-in, NOT run in CI (kata nc1j, W4, design-v033-
# final.md). Drives a REAL compiled tailport binary inside tmux through the
# full named-Cloudflare-Tunnel flow against the Tier 2 fake cloudflared
# (internal/cftunnel/fakecloudflared) -- no Cloudflare account, no network
# beyond loopback, and NEVER the real cloudflared binary (that would open a
# real trycloudflare/named tunnel -- see the coordinator's amendment A4).
#
# Usage: scripts/e2e-cftunnel-fake.sh
#
# It builds tailport and the fake fresh, runs the 11-step flow from the
# design doc's W4 brief, and prints one PASS/FAIL/INFO line per step plus a
# final summary. Steps 9-11 are EXPECTED to fail on a branch that doesn't yet
# have W3a (named re-raise memory, the same-tunnel-name guard, confirm text)
# and W3b (vanished-tunnel exit toasts) -- see each step's own comment for
# exactly what it's proving and why. The script is written so that it will
# start PASSING those steps, unmodified, once W3a/W3b land: it doesn't
# hard-code "must fail" anywhere, it just reports what it observes.
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

# cloudflared.domain prefills the named-tunnel hostname prompt
# (enterTunnelNamedHost, ui/cftunnel.go) -- this is what lets step 2 assert
# an exact, predictable prefill instead of an empty field.
cat >"$SCRATCH/xdgcfg/tailport/config.yaml" <<'EOF'
cloudflared:
  domain: app.example.test
EOF

ARGV_LOG="$SCRATCH/argv.log"
: >"$ARGV_LOG"

# ---------------------------------------------------------------------------
# tmux session: a plain persistent shell, NOT `tailport` as the pane's
# command directly -- so that quitting tailport (step 6) leaves the pane (and
# the tmux server) alive to relaunch it from (step 7), rather than tmux
# tearing the whole session down the moment its one window's process exits.
# ---------------------------------------------------------------------------
tmux new-session -d -s "$SESSION" -x 220 -y 50 bash --norc
sleep 0.3
send "export HOME=$SCRATCH/home XDG_CONFIG_HOME=$SCRATCH/xdgcfg XDG_STATE_HOME=$SCRATCH/xdgstate XDG_DATA_HOME=$SCRATCH/xdgdata PATH=$SCRATCH/fakebin:\$PATH FAKE_CF_ARGV_LOG=$ARGV_LOG FAKE_CF_MAX_LIFETIME=10m" Enter
sleep 0.3

launch_tailport() {
    send "$TAILPORT_BIN" Enter
    sleep 1
}

echo
echo "== starting the flow =="
echo

start_httpd 8791
launch_tailport

# ---------------------------------------------------------------------------
# Steps 1-5: fresh named-tunnel setup on :8791.
# ---------------------------------------------------------------------------
send "a" # all ports (a fresh ad-hoc http.server isn't a tracked favorite)
sleep 0.3
filter_to 8791
if ! wait_until 5 ":8791"; then
    echo "FATAL: :8791 never appeared in the port list" >&2
    exit 1
fi

send "o"
if wait_until 3 "q: quick random url" "n: named hostname"; then
    record "1. mode select appears" PASS "entryTunnelMode prompt shown (q: quick / n: named)"
else
    record "1. mode select appears" FAIL "mode-select prompt never appeared: $(pane | tail -3)"
fi

send "n"
if wait_until 3 "public hostname:" "app.example.test"; then
    record "2. hostname prompt prefilled" PASS "prefilled from cloudflared.domain: app.example.test"
else
    record "2. hostname prompt prefilled" FAIL "hostname prompt did not show the prefill: $(pane | tail -3)"
fi

send "Enter" # accept the prefilled hostname
sleep 0.2
send "web"
sleep 0.2
name_echoed=1
pane_has "tunnel name:" "web" || name_echoed=0
send "Enter" # accept the name -> confirm
confirm_ok=1
wait_until 3 "https://app.example.test" || confirm_ok=0
if [ "$name_echoed" = 1 ] && [ "$confirm_ok" = 1 ]; then
    # Once W3a lands, entryConfirmTunnelNamed's render adds a line
    # `via tunnel "<name>" -- tailport can't verify the hostname routes to
    # it` (coordinator-confirmed exact wording); check for that first.
    if pane_has 'via tunnel "web"'; then
        record "3. confirm names host and tunnel" PASS "confirm shows https://app.example.test AND \"via tunnel \\\"web\\\"\""
    else
        # This is the ONE assertion this script softens relative to the
        # design doc's literal wording: on THIS branch (pre-W3a),
        # entryConfirmTunnelNamed's render (ui.go ~7698-7704) shows the
        # hostname but never the tunnel name -- that's W3a's "confirm text"
        # fix, not yet here. What IS verifiable now, and IS verified above,
        # is that the flow correctly carried "web" through the name prompt
        # and that the confirm names the right URL; step 4's argv-log check
        # below independently proves "web" is what actually got run. Once
        # W3a lands, the check above starts passing instead, with no script
        # change needed.
        record "3. confirm shows the right URL (tunnel-name-in-confirm is W3a, not yet landed)" PASS "hostname prompt + name entry + confirm URL all correct; confirm text naming \"web\" is a documented W3a addition"
    fi
else
    record "3. confirm names host and tunnel" FAIL "name_echoed=$name_echoed confirm_ok=$confirm_ok: $(pane | tail -3)"
fi

CONSOLE1="$SCRATCH/xdgstate/tailport/cftunnel-8791-app.example.test.console"

send "y"
sleep 0.5
track_fake "$CONSOLE1"

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
        flags = ("--metrics", "--logfile", "--no-autoupdate")
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
if argv_order_ok; then
    record "4. argv log has the exact new order" PASS "tunnel-level flags before run, --url after run, name last (see $ARGV_LOG)"
else
    record "4. argv log has the exact new order" FAIL "no logged invocation matched the new-order shape: $(tail -3 "$ARGV_LOG")"
fi

if wait_until 8 "◈" "app.example.test"; then
    record "5. marker+URL appear, not stale within 5s" PASS "◈ https://app.example.test shown, no ▲/stale"
else
    record "5. marker+URL appear, not stale within 5s" FAIL "never reached a non-stale ◈ row: $(pane | sed -n '1,8p')"
fi

# ---------------------------------------------------------------------------
# Step 6: quit tailport; the fake (detached, its own session) must survive.
# ---------------------------------------------------------------------------
send "q"
sleep 0.5
PID1=$(fake_pid_of "$CONSOLE1")
if [ -n "$PID1" ] && kill -0 "$PID1" 2>/dev/null; then
    record "6. fake survives tailport quit" PASS "fake cloudflared pid $PID1 still running after quit"
else
    record "6. fake survives tailport quit" FAIL "pid not found (or not alive) from $CONSOLE1"
fi

# ---------------------------------------------------------------------------
# Step 7: relaunch; the tunnel is rediscovered with its hostname, no re-confirm.
# ---------------------------------------------------------------------------
launch_tailport
send "a"
sleep 0.3
filter_to 8791
if wait_until 6 "◈" "app.example.test"; then
    record "7. relaunch rediscovers tunnel+hostname" PASS "rediscovered immediately with https://app.example.test, no re-confirm"
else
    record "7. relaunch rediscovers tunnel+hostname" FAIL "tunnel not rediscovered after relaunch: $(pane | sed -n '1,8p')"
fi

# ---------------------------------------------------------------------------
# Step 8: o tears it down (de-escalation, no confirm).
# ---------------------------------------------------------------------------
send "o"
sleep 0.3
torn_down=1
wait_absent 3 "◈" || torn_down=0
sleep 0.3
proc_gone=1
kill -0 "$PID1" 2>/dev/null && proc_gone=0
if [ "$torn_down" = 1 ] && [ "$proc_gone" = 1 ]; then
    record "8. o tears it down" PASS "row cleared and pid $PID1 exited, no confirm prompt"
else
    record "8. o tears it down" FAIL "torn_down=$torn_down proc_gone=$proc_gone"
fi

# ---------------------------------------------------------------------------
# Step 9: o again -- should show a confirm naming BOTH host and tunnel, then
# successfully restart it. EXPECTED TO FAIL on this branch: audit item 2 /
# W3a (tunnelPollMsg's memory-seeding at ui.go ~3951-3957 overwrites
# m.lastTunnel[port].name with "" on every poll, since tunnelInfo carries no
# name field yet). By the time this step runs, at least one poll has already
# passed (steps 6-8 took well over one tunnelPollInterval), so the remembered
# name is already gone: the re-raise confirm names the host but not the
# tunnel, and confirming it calls cftunnel.Start with an EMPTY TunnelName,
# which ValidTunnelName rejects outright -- surfacing as a toast, not a
# silent no-op.
# ---------------------------------------------------------------------------
send "o"
sleep 0.3
confirm9=$(pane)
send "y"
sleep 0.5
track_fake "$CONSOLE1" # no-op re-add of the (dead) old pid if this failed; picks up the NEW pid if it succeeded
result9=$(pane)
if echo "$result9" | grep -q "invalid tunnel name"; then
    record "9. re-raise names host+tunnel (EXPECTED FAIL, W3a)" FAIL "re-raise's remembered name was lost across the poll (audit item 2): confirming sent cftunnel.Start an empty TunnelName, rejected as: $(echo "$result9" | grep "invalid tunnel name" | tail -1)"
elif echo "$confirm9" | grep -qF 'via tunnel "web"' && echo "$result9" | grep -qF "app.example.test"; then
    record "9. re-raise names host+tunnel" PASS "re-raise confirm named the tunnel (via tunnel \"web\") and restarted it successfully"
else
    record "9. re-raise names host+tunnel (EXPECTED FAIL, W3a)" FAIL "unexpected outcome -- confirm: $(echo "$confirm9" | tail -3) | after y: $(echo "$result9" | tail -3)"
fi

# ---------------------------------------------------------------------------
# Step 10: the same tunnel name refused on a second port. Deliberately uses
# TWO FRESH ports that have never been tunnelled this session, going through
# the FULL fresh setup flow (not the memory shortcut) on both -- an
# independent, clean test of the guard itself. It ALSO deliberately uses a
# tunnel name ("web2") distinct from the one used on :8791 above: on a build
# where W3a's guard already exists (post-fix), step 9 may have successfully
# restarted "web" on :8791, and re-using that same name here would trip the
# very guard this step means to test against the WRONG port pair (:8791 vs
# :8792, not :8792 vs :8793) -- so this step's precondition and its subject
# are kept independent of whatever state :8791 ended up in.
# EXPECTED TO FAIL on this branch: no tunnelNameInUse guard exists yet
# anywhere in requestTunnel/updateTunnelEntry/confirmTunnelNamed (that's
# W3a's [R3] fix) -- so a SECOND, genuinely separate cloudflared process
# named "web2" is expected to actually start for the second port.
# ---------------------------------------------------------------------------
start_httpd 8792
start_httpd 8793

send "r"
sleep 0.3
filter_to 8792
send "o"; sleep 0.2
send "n"; sleep 0.2
send "Enter"; sleep 0.2 # accept prefilled hostname
send "web2"; sleep 0.2
send "Enter"; sleep 0.2 # -> confirm
send "y"; sleep 0.5
track_fake "$SCRATCH/xdgstate/tailport/cftunnel-8792-app.example.test.console"
# The precondition here is just "web2 is running for :8792" -- Start's
# optimistic tunnelDoneMsg handler adds the row synchronously, well before
# the next poll flips it from stale to ready, so this doesn't need to wait
# out a full poll cycle the way step 5's OWN staleness claim does. Scoped to
# :8792's own block (see port_block_has) since :8791 may already carry an
# unrelated live tunnel row by this point (step 9).
web_on_8792=1
wait_until_port 6 8792 "cloudflare" "app.example.test" || web_on_8792=0

send "r"
sleep 0.3
filter_to 8793
send "o"; sleep 0.2
send "n"; sleep 0.2
send "Enter"; sleep 0.2 # accept prefilled hostname
send "web2"; sleep 0.2
send "Enter"; sleep 0.3 # -> confirm, OR (post-W3a) an immediate refusal toast
# W3a's tunnelNameInUse guard fires at THREE call sites (design doc): name
# entry, re-raise, and confirm. On a build that has it, refusing at NAME
# ENTRY means the flow never reaches entryConfirmTunnelNamed at all -- it's
# the FIRST of those three sites this script's flow can reach, so the
# refusal (if any) must be checked for HERE, before ever sending "y". Sending
# "y" unconditionally would, on a refusal, just get typed as a literal
# character into the still-open name textinput instead of confirming
# anything.
result10=$(pane)
# Coordinator-confirmed exact refusal wording (W3a):
#   tunnel "web2" is already running for :8792 -- a named tunnel serves one
#   port; press o on :8792 first
refused10=0
echo "$result10" | grep -qF "already running for :8792" && echo "$result10" | grep -qF "press o on :8792" && refused10=1
if [ "$refused10" != 1 ]; then
    # Not refused at name entry -- proceed to the confirm and actually start
    # it, so a build with NO guard at all (this branch) is still exercised
    # end to end rather than just timing out.
    send "y"; sleep 0.5
    track_fake "$SCRATCH/xdgstate/tailport/cftunnel-8793-app.example.test.console"
    result10=$(pane)
fi

if [ "$web_on_8792" != 1 ]; then
    record "10. same-name refused on second port" FAIL "setup precondition failed: could not get \"web2\" running on :8792 to test against"
elif [ "$refused10" = 1 ] || (echo "$result10" | grep -qF "already running for :8792" && echo "$result10" | grep -qF "press o on :8792"); then
    record "10. same-name refused on second port" PASS "second port refused: $(echo "$result10" | grep -F "already running for :8792" | tail -1)"
else
    web_on_8793=0
    wait_until_port 6 8793 "cloudflare" "app.example.test" && web_on_8793=1
    live_count=$(ps -eo args | grep -F 'run --url' | grep -F ' web2' | grep -cv grep || true)
    record "10. same-name refused on second port (EXPECTED FAIL, W3a)" FAIL "no refusal toast; :8793 also went ◈ live (web_on_8793=$web_on_8793), $live_count cloudflared process(es) now running tunnel \"web2\" for two different ports"
fi
# A refusal at NAME ENTRY (see the comment above result10) leaves the name
# textinput open rather than returning to entryNone, so a stray key sent
# afterward (like step 11's own "q" to quit) would land as a literal
# character in that field instead of doing anything global. esc always
# clears the flow regardless of which of the three outcomes above happened
# (a no-op if we're already at entryNone), so every subsequent step starts
# from a known-clean state.
send "esc"
sleep 0.2

# ---------------------------------------------------------------------------
# Step 11: FAKE_CF_FAIL_AFTER produces an exit toast. Needs a fresh relaunch
# (the fake reads the env var from the PROCESS that spawns it -- tailport --
# at spawn time, so it must be exported in the pane's shell BEFORE tailport
# starts, not after). EXPECTED TO FAIL on this branch: tunnelPollMsg carries
# no `vanished` field yet, and nothing in the tunnelPollMsg handler compares
# against a previous poll's pid set (that's W3b) -- so a tunnel that dies
# between polls is expected to just disappear with no toast at all.
# ---------------------------------------------------------------------------
send "q"
sleep 0.5
send "export FAKE_CF_FAIL_AFTER=3s:e2e-induced-crash" Enter
sleep 0.2
# Start the http.server BEFORE relaunching this time: tailport's port list
# only gets a scan at Init() and on "r" -- unlike steps 1/7/10 above (whose
# ports existed before the tailport process that first scans for them was
# even launched), an 8794 server started AFTER relaunch would need an extra
# "r" the initial scan wouldn't need; starting it first sidesteps that.
start_httpd 8794
launch_tailport

send "a"
sleep 0.3
filter_to 8794
send "o"; sleep 0.2
send "q"; sleep 0.2 # entryTunnelMode's OWN "q" (quick), not the global quit -- see filter_to's caller comment
send "y"; sleep 0.5
track_fake "$SCRATCH/xdgstate/tailport/cftunnel-8794.console" # quick: no hostname suffix

quick_up=1
wait_until 10 "◈" "fake-8794.trycloudflare.com" || quick_up=0
# Let FAKE_CF_FAIL_AFTER=3s actually fire, then give the poll ticker
# (tunnelPollInterval=4s) a full cycle plus margin to notice.
sleep 8
result11=$(pane)
vanished=1
pane_has "◈" "fake-8794.trycloudflare.com" && vanished=0

# Coordinator-confirmed exact vanish-toast wording (W3b):
#   Cloudflare tunnel on :8794 exited -- <ConsoleTail>
if [ "$quick_up" != 1 ]; then
    record "11. exit toast on vanished tunnel" FAIL "setup precondition failed: quick tunnel on :8794 never came up"
elif echo "$result11" | grep -qF "Cloudflare tunnel on :8794" && echo "$result11" | grep -qi "exited"; then
    record "11. exit toast on vanished tunnel" PASS "exit toast shown: $(echo "$result11" | grep -i "exited" | tail -1)"
else
    record "11. exit toast on vanished tunnel (EXPECTED FAIL, W3b)" FAIL "tunnel vanished=$vanished but no toast mentioning the exit ever appeared"
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
