package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gruen/tailport/internal/selfupdate"
)

// Undocumented env test seams, mirroring install.sh's TAILPORT_BASE_URL: they
// point the self-updater at a local httptest.Server (and a throwaway install
// target) so the compiled binary can be exercised end-to-end without the
// network and without overwriting itself. Not a supported/public knob.
const (
	envUpdateAPIURL      = "TAILPORT_UPDATE_API_URL"      // overrides the GitHub API base
	envUpdateDownloadURL = "TAILPORT_UPDATE_DOWNLOAD_URL" // overrides the release-download base
	envUpdateExec        = "TAILPORT_UPDATE_EXEC"         // overrides the install target (os.Executable)
)

// updateFlags are the update subcommand's own flags, mirroring kata update:
// --check (report only), -y/--yes (no prompt), -f/--force (act even if already
// latest / override the package-managed refusal).
type updateFlags struct {
	checkOnly bool
	assumeYes bool
	force     bool
}

// runUpdate implements `tailport update`. args is everything after the
// subcommand name (i.e. os.Args[2:]). It parses the update flags, wires an
// Updater to the real endpoints (or the env test seams), resolves the running
// binary's path, and hands off to updateRun, which is the pure, injectable core
// the tests drive against an httptest.Server.
func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tailport update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	var uf updateFlags
	fs.BoolVar(&uf.checkOnly, "check", false, "report whether an update is available; do not install")
	fs.BoolVar(&uf.assumeYes, "yes", false, "install without prompting")
	fs.BoolVar(&uf.assumeYes, "y", false, "install without prompting (shorthand)")
	fs.BoolVar(&uf.force, "force", false, "check/reinstall even if already up to date; override the package-managed refusal")
	fs.BoolVar(&uf.force, "f", false, "shorthand for --force")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUpdateUsage(stdout)
			return 0
		}
		fmt.Fprintln(stderr, "tailport update:", err)
		printUpdateUsage(stderr)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tailport update: unexpected argument %q\n", fs.Arg(0))
		printUpdateUsage(stderr)
		return 2
	}

	u := selfupdate.NewUpdater(version)
	if v := os.Getenv(envUpdateAPIURL); v != "" {
		u.APIBase = v
	}
	if v := os.Getenv(envUpdateDownloadURL); v != "" {
		u.DownloadBase = v
	}

	execPath, execErr := resolveExecPath()
	if !uf.checkOnly && execErr != nil {
		// Only the install path needs to know where the binary lives; --check
		// never touches disk, so don't fail it on an os.Executable() hiccup.
		fmt.Fprintln(stderr, "tailport update: cannot locate the running binary:", execErr)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	return updateRun(ctx, u, uf, execPath, os.Stdin, stdout, stderr)
}

// resolveExecPath returns the path to replace. The env seam short-circuits to
// its literal value (the test harness points it at a throwaway copy). Otherwise
// it uses os.Executable() and resolves symlinks, so package-manager detection
// sees the real on-disk location (e.g. a Homebrew Cellar target) rather than a
// PATH symlink.
func resolveExecPath() (string, error) {
	if v := os.Getenv(envUpdateExec); v != "" {
		return v, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// updateRun is the orchestration core, split out from runUpdate so tests can
// inject the Updater, flags, target path, and stdin without spawning a process.
// It returns the process exit code.
func updateRun(ctx context.Context, u *selfupdate.Updater, uf updateFlags, execPath string, in io.Reader, stdout, stderr io.Writer) int {
	check, err := u.Check(ctx)
	if err != nil {
		printCheckError(stderr, err)
		return 1
	}

	// Already current and not forcing: report and stop. This is also the
	// up-to-date answer for `--check`.
	if !check.Available && !uf.force {
		fmt.Fprintf(stdout, "tailport %s is up to date (latest is %s).\n", displayVersion(check.Current), check.Latest)
		return 0
	}

	if check.Available {
		if check.CurrentIsDev {
			fmt.Fprintf(stdout, "Update available: %s (unversioned build) -> %s\n", displayVersion(check.Current), check.Latest)
		} else {
			fmt.Fprintf(stdout, "Update available: %s -> %s\n", displayVersion(check.Current), check.Latest)
		}
	} else {
		fmt.Fprintf(stdout, "tailport is already at the latest version (%s).\n", check.Latest)
	}

	// --check never installs, whatever else was asked.
	if uf.checkOnly {
		return 0
	}

	if !check.Available && uf.force {
		fmt.Fprintln(stdout, "Reinstalling due to --force.")
	}

	// Package-managed refusal (overridable with --force, but still noted).
	if pm := selfupdate.DetectPkgManager(execPath); pm.Managed() {
		printPkgManagedNote(stderr, execPath, pm)
		if !uf.force {
			return 1
		}
		fmt.Fprintln(stderr, "tailport update: --force given -- self-replacing anyway.")
	}

	if !uf.assumeYes {
		ok, err := confirm(in, stdout, fmt.Sprintf("Download and install %s? [y/N] ", check.Latest))
		if err != nil {
			fmt.Fprintln(stderr, "tailport update:", err)
			return 1
		}
		if !ok {
			fmt.Fprintln(stdout, "Update cancelled.")
			return 0
		}
	}

	fmt.Fprintf(stdout, "Downloading %s ...\n", selfupdate.AssetName(u.GOOS, u.GOARCH))
	bin, err := u.Download(ctx)
	if err != nil {
		if errors.Is(err, selfupdate.ErrChecksumMismatch) {
			fmt.Fprintln(stderr, "tailport update: checksum verification FAILED -- refusing to install.")
			fmt.Fprintln(stderr, "tailport update: your existing tailport is untouched.")
			return 1
		}
		fmt.Fprintln(stderr, "tailport update: download failed:", err)
		return 1
	}

	if err := selfupdate.ReplaceExecutable(execPath, bin, 0o755); err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintf(stderr, "tailport update: no write permission to %s; re-run with sudo or reinstall.\n", filepath.Dir(execPath))
			fmt.Fprintln(stderr, "tailport update: your existing tailport is untouched.")
			return 1
		}
		fmt.Fprintln(stderr, "tailport update: install failed:", err)
		return 1
	}

	fmt.Fprintf(stdout, "Updated tailport to %s (%s).\n", check.Latest, execPath)
	return 0
}

// confirm prints prompt and reads a yes/no answer from in. A bare Enter, EOF,
// or anything other than y/yes (case-insensitive) is treated as "no", so a
// non-interactive `tailport update` never installs without an explicit yes.
func confirm(in io.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// displayVersion renders a version for humans: a "dev" build is spelled out so
// the message isn't a cryptic "dev is up to date".
func displayVersion(v string) string {
	if v == "" || v == "dev" {
		return "dev"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

func printCheckError(w io.Writer, err error) {
	switch {
	case errors.Is(err, selfupdate.ErrNoRelease):
		fmt.Fprintln(w, "tailport update: no published release found yet -- there may be no tagged release to update to.")
	case errors.Is(err, selfupdate.ErrRateLimited):
		fmt.Fprintln(w, "tailport update: GitHub API rate limit exceeded; please try again later.")
	default:
		fmt.Fprintln(w, "tailport update: could not check for updates:", err)
	}
}

func printPkgManagedNote(w io.Writer, execPath string, pm selfupdate.PkgManager) {
	fmt.Fprintf(w, "tailport at %s looks installed via %s.\n", execPath, pm.Name)
	fmt.Fprintf(w, "Update it with your package manager (%s).\n", pm.Advice)
	fmt.Fprintln(w, "If you ALSO have a curl-installed copy, run `command -v -a tailport` and check your PATH -- the updated one may not be first.")
}

func printUpdateUsage(w io.Writer) {
	fmt.Fprintln(w, "tailport update -- self-update tailport to the latest GitHub release.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  tailport update            check for a newer release, then prompt to install")
	fmt.Fprintln(w, "  tailport update --check    report whether an update is available; do not install")
	fmt.Fprintln(w, "  tailport update -y|--yes   install without prompting")
	fmt.Fprintln(w, "  tailport update -f|--force check/reinstall even if already up to date")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The download is sha256-verified before the running binary is atomically")
	fmt.Fprintln(w, "replaced. Package-managed installs (Homebrew, pacman/AUR) are left for the")
	fmt.Fprintln(w, "package manager unless --force is given.")
}
