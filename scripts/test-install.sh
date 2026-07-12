#!/bin/sh
# Verification harness for install.sh (kata 4nas): exercises the download ->
# checksum-verify -> install path against a local file:// mock, with no
# network and no touching the real $HOME. POSIX sh so it runs under dash too;
# on Debian/Ubuntu (and GitHub's ubuntu runners) /bin/sh IS dash, which is
# the real-dash coverage this box (bash-as-sh) can't provide.
#
#   sh scripts/test-install.sh                 # from the repo root
#   INSTALL_SH_PATH=/abs/install.sh sh scripts/test-install.sh
#   KEEP_TMP=1 sh scripts/test-install.sh      # leave the workdir for inspection
#
# SC2030/SC2031: env vars (HOME, TAILPORT_*) are deliberately scoped to each
# case's ( ... ) subshell so cases don't leak state into one another -- the
# "modification is local to the subshell" is the intent, not a bug.
# shellcheck disable=SC2030,SC2031
set -eu

INSTALL_SH="${INSTALL_SH_PATH:-$(pwd)/install.sh}"
KEEP_TMP="${KEEP_TMP:-0}"
FAILED=0

pass() { echo "[PASS] $1"; }
fail() {
	echo "[FAIL] $1" >&2
	FAILED=$((FAILED + 1))
}

[ -f "$INSTALL_SH" ] || {
	echo "cannot find install.sh at $INSTALL_SH" >&2
	exit 2
}

WORKDIR="$(mktemp -d)"
# shellcheck disable=SC2317  # invoked indirectly via the EXIT/INT/TERM trap below
cleanup() {
	if [ "$KEEP_TMP" = "1" ]; then
		echo "KEEP_TMP=1: leaving $WORKDIR in place" >&2
	else
		rm -rf "$WORKDIR"
	fi
}
trap cleanup EXIT INT TERM

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{ print $NF }'
	else
		echo "no sha256 tool on the test runner" >&2
		exit 2
	fi
}

# This machine's real OS/arch, so install.sh's own uname detection runs for
# real -- only the network fetch is mocked.
uos="$(uname -s)"
case "$uos" in
Linux) mgoos="linux" ;;
Darwin) mgoos="darwin" ;;
*)
	echo "unsupported OS for this harness: $uos" >&2
	exit 2
	;;
esac
uarch="$(uname -m)"
case "$uarch" in
x86_64 | amd64) mgoarch="amd64" ;;
arm64 | aarch64) mgoarch="arm64" ;;
*)
	echo "unsupported arch for this harness: $uarch" >&2
	exit 2
	;;
esac
ASSET="tailport-${mgoos}-${mgoarch}"

# build_mock <root> <latest|pinned> <version-or-empty> <printed-text>
# Lays out a release tree matching install.sh's URL scheme, where
# TAILPORT_BASE_URL stands in for ".../<repo>/releases".
build_mock() {
	root="$1"
	mode="$2"
	ver="$3"
	text="$4"
	if [ "$mode" = latest ]; then
		dir="$root/latest/download"
	else
		dir="$root/download/v${ver#v}"
	fi
	mkdir -p "$dir"
	printf '#!/bin/sh\necho %s\n' "$text" >"$dir/$ASSET"
	chmod +x "$dir/$ASSET"
	printf '%s  %s\n' "$(sha256_of "$dir/$ASSET")" "$ASSET" >"$dir/$ASSET.sha256"
}

# run_install <shell> <mock-root> [extra env assignments...] -- runs install.sh
# with TAILPORT_BASE_URL pointed at the mock; returns install.sh's exit code.
# Extra args are exported env assignments (e.g. TAILPORT_INSTALL_DIR=/x).
run_install() {
	rshell="$1"
	rroot="$2"
	shift 2
	(
		for kv in "$@"; do
			# shellcheck disable=SC2163  # intentional dynamic export of KEY=VALUE
			export "$kv"
		done
		export TAILPORT_BASE_URL="file://$rroot"
		"$rshell" "$INSTALL_SH"
	)
}

echo "== static checks =="
if grep -nE '^[^#]*set[^#]*pipefail' "$INSTALL_SH" >/dev/null 2>&1; then
	fail "install.sh still sets pipefail (breaks real dash)"
else
	pass "no 'set ... pipefail'"
fi
if sh -n "$INSTALL_SH"; then pass "sh -n"; else fail "sh -n"; fi
if command -v bash >/dev/null 2>&1; then
	if bash -n "$INSTALL_SH"; then pass "bash -n"; else fail "bash -n"; fi
fi
if command -v dash >/dev/null 2>&1; then
	if dash -n "$INSTALL_SH"; then pass "dash -n (real dash)"; else fail "dash -n"; fi
else
	echo "[SKIP] dash absent: this 'sh' is bash-as-sh, which accepts 'set -o pipefail'." >&2
	echo "       Real-dash coverage comes from CI (ubuntu /bin/sh is dash)." >&2
fi
if command -v shellcheck >/dev/null 2>&1; then
	if shellcheck "$INSTALL_SH"; then pass "shellcheck clean"; else fail "shellcheck"; fi
else
	echo "[SKIP] shellcheck absent here; runs in CI (preinstalled on ubuntu runners)." >&2
fi

echo "== mocks =="
GOOD="$WORKDIR/good"
BAD="$WORKDIR/bad"
build_mock "$GOOD" latest "" tailport-fake
build_mock "$GOOD" pinned 9.9.9 tailport-fake-pinned
build_mock "$BAD" latest "" tailport-fake
# Corrupt the bad binary but leave its now-stale .sha256, forcing a mismatch.
printf '#!/bin/sh\necho corrupted\n' >"$BAD/latest/download/$ASSET"
pass "built good + bad + pinned mocks for $ASSET"

assert_installed() {
	dest="$1"
	want="$2"
	if [ -x "$dest" ]; then
		pass "installed + executable: $dest"
	else
		fail "missing/not executable: $dest"
		return
	fi
	got="$("$dest" 2>&1 || true)"
	if [ "$got" = "$want" ]; then
		pass "runs and prints '$want'"
	else
		fail "output mismatch: got '$got' want '$want'"
	fi
}

echo "== happy path: sh, default \$HOME/.local/bin =="
H1="$WORKDIR/home1"
mkdir -p "$H1"
if run_install sh "$GOOD" "HOME=$H1"; then pass "sh install exit 0"; else fail "sh install nonzero"; fi
assert_installed "$H1/.local/bin/tailport" tailport-fake

echo "== pipe shape: cat install.sh | sh =="
H2="$WORKDIR/home2"
mkdir -p "$H2"
if (
	export HOME="$H2" TAILPORT_BASE_URL="file://$GOOD"
	# shellcheck disable=SC2002  # the cat|sh pipe is the point: it mirrors `curl ... | sh`
	cat "$INSTALL_SH" | sh
); then
	pass "piped-to-sh install exit 0"
else
	fail "piped-to-sh install nonzero (the exact bug 4nas fixes)"
fi
assert_installed "$H2/.local/bin/tailport" tailport-fake

echo "== idempotency: re-run over the same dest =="
before="$(sha256_of "$H1/.local/bin/tailport")"
run_install sh "$GOOD" "HOME=$H1" >/dev/null 2>&1 || fail "idempotent re-run nonzero"
after="$(sha256_of "$H1/.local/bin/tailport")"
if [ "$before" = "$after" ]; then pass "re-run byte-identical"; else fail "re-run changed dest"; fi

echo "== bash direct + TAILPORT_INSTALL_DIR override =="
if command -v bash >/dev/null 2>&1; then
	H3="$WORKDIR/home3"
	DIR3="$WORKDIR/custom-dir"
	mkdir -p "$H3"
	if run_install bash "$GOOD" "HOME=$H3" "TAILPORT_INSTALL_DIR=$DIR3"; then
		pass "bash install exit 0"
	else
		fail "bash install nonzero"
	fi
	assert_installed "$DIR3/tailport" tailport-fake
else
	echo "[SKIP] bash absent" >&2
fi

echo "== TAILPORT_VERSION pinning =="
H4="$WORKDIR/home4"
mkdir -p "$H4"
if run_install sh "$GOOD" "HOME=$H4" "TAILPORT_VERSION=v9.9.9"; then
	pass "pinned install exit 0"
else
	fail "pinned install nonzero"
fi
assert_installed "$H4/.local/bin/tailport" tailport-fake-pinned

echo "== checksum mismatch aborts, creates no dest =="
H5="$WORKDIR/home5"
mkdir -p "$H5"
set +e
run_install sh "$BAD" "HOME=$H5" >/dev/null 2>&1
rc=$?
set -e
if [ "$rc" -ne 0 ]; then pass "mismatch exit nonzero ($rc)"; else fail "mismatch exit 0"; fi
if [ ! -e "$H5/.local/bin/tailport" ]; then pass "no dest created on mismatch"; else fail "dest created on mismatch"; fi

echo "== mismatch does not clobber an existing good install =="
H6="$WORKDIR/home6"
mkdir -p "$H6"
run_install sh "$GOOD" "HOME=$H6" >/dev/null 2>&1 || fail "seed good install failed"
good_hash="$(sha256_of "$H6/.local/bin/tailport")"
set +e
run_install sh "$BAD" "HOME=$H6" >/dev/null 2>&1
rc=$?
set -e
if [ "$rc" -ne 0 ]; then pass "corrupt re-run exit nonzero"; else fail "corrupt re-run exit 0"; fi
after_hash="$(sha256_of "$H6/.local/bin/tailport")"
if [ "$good_hash" = "$after_hash" ]; then pass "existing install left intact"; else fail "existing install clobbered"; fi

echo
echo "================ SUMMARY ================"
if [ "$FAILED" -eq 0 ]; then
	echo "ALL CHECKS PASSED"
	exit 0
else
	echo "$FAILED CHECK(S) FAILED"
	exit 1
fi
