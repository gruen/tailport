// Package selfupdate implements the mechanism behind `tailport update`: it
// asks the GitHub Releases API for the latest tag, compares it to the running
// build's version, downloads the per-arch asset, verifies its published
// sha256 BEFORE touching anything on disk, and atomically swaps the running
// binary in place.
//
// Everything here is pure Go on the standard library (net/http, crypto/sha256,
// encoding/json, os) per the project's zero-non-Go-runtime-deps rule, and is
// built for injectability: the HTTP client and the API/download base URLs are
// fields on Updater, so tests drive the whole flow against an httptest.Server
// and never touch the network. The CLI glue (prompting, messaging, env test
// seams) lives in cmd/tailport/update.go; this package only computes and acts.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub "owner/name" tailport releases from. Kept in one place so
// the API endpoint and the download base can't drift apart.
const Repo = "gruen/tailport"

// maxDownload caps any single response body read so a hostile or broken server
// can't make tailport allocate unbounded memory. The largest thing fetched is
// the per-arch binary (a few MB); 256 MiB is comfortably above that.
const maxDownload = 256 << 20

// Sentinel errors let the CLI map failure modes onto specific, friendly
// messages (and specific exit behavior) with errors.Is, instead of matching on
// error strings.
var (
	// ErrNoRelease is returned when the API reports no latest release (404):
	// the first tagged release may simply not exist yet.
	ErrNoRelease = errors.New("no published release found")
	// ErrRateLimited is returned when GitHub rejects the request for rate
	// limiting (403/429 with the rate-limit signal).
	ErrRateLimited = errors.New("GitHub API rate limit exceeded")
	// ErrChecksumMismatch is returned by Download when the downloaded asset's
	// sha256 does not match the published checksum. When this fires, NOTHING
	// has been written over the installed binary.
	ErrChecksumMismatch = errors.New("checksum verification failed")
)

// Version is a parsed semantic version: the three numeric components plus an
// optional prerelease tag (the part after "-"). Build metadata (after "+") is
// parsed off and ignored, per semver, since it never affects precedence.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// ParseVersion parses "vX.Y.Z", "X.Y.Z", or either with a "-prerelease" suffix
// (and tolerates a trailing "+build" which it discards). A leading "v"/"V" is
// stripped, so the git tag ("v0.1.0") and the ldflags-injected main.version
// ("0.1.0") parse to the same Version. It rejects anything that isn't three
// dotted non-negative integers — notably "dev", which is how an unreleased
// local build identifies itself.
func ParseVersion(s string) (Version, error) {
	orig := s
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 { // drop build metadata
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 { // split off prerelease
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("not a semantic version: %q", orig)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("not a semantic version: %q", orig)
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, nil
}

// Compare reports whether a is older (-1), equal (0), or newer (+1) than b,
// following semver precedence: numeric fields first, then prerelease handling
// where a version WITH a prerelease tag ranks below the same version without
// one (1.0.0-rc1 < 1.0.0).
func Compare(a, b Version) int {
	if c := cmpInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := cmpInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := cmpInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	return comparePre(a.Pre, b.Pre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePre implements semver prerelease precedence: no prerelease outranks
// any prerelease; otherwise compare dot-separated identifiers, numeric ones
// numerically and mixed ones with numeric < alphanumeric, and a longer set of
// identifiers outranks a shorter prefix-equal one.
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	ai, bi := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ai) && i < len(bi); i++ {
		an, aIsNum := atoiOK(ai[i])
		bn, bIsNum := atoiOK(bi[i])
		switch {
		case aIsNum && bIsNum:
			if c := cmpInt(an, bn); c != 0 {
				return c
			}
		case aIsNum && !bIsNum:
			return -1 // numeric identifiers have lower precedence
		case !aIsNum && bIsNum:
			return 1
		default:
			if ai[i] < bi[i] {
				return -1
			}
			if ai[i] > bi[i] {
				return 1
			}
		}
	}
	return cmpInt(len(ai), len(bi))
}

func atoiOK(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// AssetName is the release asset filename for a given GOOS/GOARCH, matching the
// names build.yml publishes and install.sh downloads (e.g. tailport-linux-amd64,
// tailport-darwin-arm64). There is no extension on any target.
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("tailport-%s-%s", goos, goarch)
}

// PkgManager describes how the running binary appears to have been installed.
// A non-empty Name means "package-managed" — tailport refuses to self-replace
// it (unless forced) and points the user at Advice instead. The zero value
// means the location looks self-installed (e.g. install.sh's ~/.local/bin) and
// is safe to atomically replace.
type PkgManager struct {
	Name   string // human label, e.g. "Homebrew"; "" == not package-managed
	Advice string // upgrade hint, e.g. "brew upgrade tailport"
}

// Managed reports whether the binary looks package-managed.
func (p PkgManager) Managed() bool { return p.Name != "" }

var (
	pmBrew   = PkgManager{Name: "Homebrew", Advice: "brew upgrade tailport"}
	pmSystem = PkgManager{Name: "a system package manager (pacman/AUR)", Advice: "pacman -Syu, or your AUR helper"}
	// pmAmbiguous covers /usr/local/bin, which is used by BOTH Homebrew and
	// hand-rolled installs. The issue says to be conservative here: warn and
	// refuse (still overridable with --force) rather than silently replacing a
	// binary that might be owned by a package manager.
	pmAmbiguous = PkgManager{Name: "a system location (/usr/local/bin is ambiguous)", Advice: "brew upgrade tailport, your system package manager, or reinstall via install.sh"}
)

// DetectPkgManager inspects the directory of execPath and classifies it. It is
// deliberately a conservative heuristic (see the issue): the strong signals are
// treated as managed, ~/.local/bin (and any other unrecognized location, such
// as a throwaway test dir) is treated as self-installed, and the genuinely
// ambiguous /usr/local/bin errs toward "managed" so tailport warns rather than
// clobbering a package-owned file.
func DetectPkgManager(execPath string) PkgManager {
	clean := filepath.Clean(execPath)
	// Homebrew keeps the real binary under .../Cellar/<formula>/<ver>/bin and
	// symlinks it onto PATH; if we resolved through to a Cellar path, it's brew
	// regardless of the leading prefix (/usr/local vs /opt/homebrew).
	if strings.Contains(clean, "/Cellar/") {
		return pmBrew
	}
	switch filepath.Dir(clean) {
	case "/opt/homebrew/bin", "/home/linuxbrew/.linuxbrew/bin", "/usr/local/Cellar":
		return pmBrew
	case "/usr/bin":
		return pmSystem
	case "/usr/local/bin":
		return pmAmbiguous
	default:
		return PkgManager{}
	}
}

// ParseChecksumFile extracts the sha256 hex digest from a published .sha256
// file. That file is GNU sha256sum's "<hex>␣␣<filename>" format (see build.yml
// and install.sh); we read only the first whitespace-delimited field, since the
// recorded filename won't match our download path anyway.
func ParseChecksumFile(b []byte) (string, error) {
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return "", errors.New("empty checksum file")
	}
	h := strings.ToLower(fields[0])
	if len(h) != 64 || !isHex(h) {
		return "", fmt.Errorf("not a sha256 digest: %q", fields[0])
	}
	return h, nil
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// VerifyChecksum computes the sha256 of data and constant-time compares it to
// expectedHex (case-insensitive). A mismatch returns ErrChecksumMismatch,
// wrapped with both digests for diagnostics.
func VerifyChecksum(data []byte, expectedHex string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	want := strings.ToLower(strings.TrimSpace(expectedHex))
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, want, got)
	}
	return nil
}

// ReplaceExecutable atomically installs data as the file at execPath. It writes
// to a fresh temp file in the SAME directory (so os.Rename is an atomic
// same-filesystem swap), chmods it to perm, then renames it over execPath. On
// any failure the temp file is removed and execPath is left byte-for-byte
// untouched — a running tailport is never truncated mid-write. When the target
// directory is not writable, the error wraps os.ErrPermission so the caller can
// print a "no write permission" message and exit cleanly.
func ReplaceExecutable(execPath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".tailport-update-*")
	if err != nil {
		// CreateTemp on a read-only dir returns a *PathError wrapping EACCES,
		// which errors.Is(..., os.ErrPermission) recognizes.
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	remove := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		remove()
		return fmt.Errorf("writing new binary: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		remove()
		return fmt.Errorf("chmod new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing new binary: %w", err)
	}
	if err := os.Rename(tmpName, execPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replacing %s: %w", execPath, err)
	}
	return nil
}

// Updater fetches release metadata and assets. Every network dependency is a
// field so tests can inject an httptest.Server: HTTPClient is the transport,
// APIBase is the GitHub API host, and DownloadBase is the release-download
// prefix that AssetName is appended to.
type Updater struct {
	Repo         string
	Current      string // running build's version (main.version), e.g. "0.1.0" or "dev"
	GOOS, GOARCH string
	HTTPClient   *http.Client
	APIBase      string // e.g. https://api.github.com
	DownloadBase string // e.g. https://github.com/gruen/tailport/releases/latest/download
}

// NewUpdater returns an Updater wired to the real GitHub endpoints for the
// current build (current is main.version). Callers may override APIBase /
// DownloadBase / HTTPClient afterward (the CLI does, from undocumented env test
// seams).
func NewUpdater(current string) *Updater {
	return &Updater{
		Repo:         Repo,
		Current:      current,
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		APIBase:      "https://api.github.com",
		DownloadBase: "https://github.com/" + Repo + "/releases/latest/download",
	}
}

func (u *Updater) client() *http.Client {
	if u.HTTPClient != nil {
		return u.HTTPClient
	}
	return http.DefaultClient
}

func (u *Updater) apiLatestURL() string {
	return strings.TrimRight(u.APIBase, "/") + "/repos/" + u.Repo + "/releases/latest"
}

func (u *Updater) assetURL(name string) string {
	return strings.TrimRight(u.DownloadBase, "/") + "/" + name
}

// get performs a GET and returns the (bounded) body and status. It always sets
// a User-Agent, which the GitHub API requires and rejects requests without.
func (u *Updater) get(ctx context.Context, url, accept string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "tailport/"+u.Current)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// LatestVersion returns the raw tag_name of the latest release (e.g. "v0.1.0"),
// mapping the notable HTTP outcomes onto sentinel errors.
func (u *Updater) LatestVersion(ctx context.Context) (string, error) {
	body, status, err := u.get(ctx, u.apiLatestURL(), "application/vnd.github+json")
	if err != nil {
		return "", fmt.Errorf("contacting GitHub: %w", err)
	}
	switch {
	case status == http.StatusNotFound:
		return "", ErrNoRelease
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return "", fmt.Errorf("%w (HTTP %d)", ErrRateLimited, status)
	case status < 200 || status >= 300:
		return "", fmt.Errorf("GitHub API returned HTTP %d", status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("parsing release JSON: %w", err)
	}
	if rel.TagName == "" {
		return "", errors.New("latest release has no tag_name")
	}
	return rel.TagName, nil
}

// Check is the result of comparing the running build to the latest release.
type Check struct {
	Current      string // running build's version string
	Latest       string // latest release tag (raw, e.g. "v0.1.0")
	Available    bool   // an update should be offered
	CurrentIsDev bool   // running build isn't a parseable release version ("dev")
}

// Check fetches the latest tag and compares it to the current build. An
// unparseable current version (a "dev" build) is treated as older than any real
// release, so Available is true and CurrentIsDev is set.
func (u *Updater) Check(ctx context.Context) (Check, error) {
	tag, err := u.LatestVersion(ctx)
	if err != nil {
		return Check{}, err
	}
	latest, err := ParseVersion(tag)
	if err != nil {
		return Check{}, fmt.Errorf("latest tag %q is not a semantic version: %w", tag, err)
	}
	res := Check{Current: u.Current, Latest: tag}
	cur, curErr := ParseVersion(u.Current)
	if curErr != nil {
		res.CurrentIsDev = true
		res.Available = true
		return res, nil
	}
	res.Available = Compare(latest, cur) > 0
	return res, nil
}

// Download fetches this machine's asset AND its published .sha256, verifies the
// asset against that checksum, and returns the verified bytes. An unverified
// binary never escapes this function: on a checksum mismatch it returns
// ErrChecksumMismatch and no data, so the caller has nothing to install.
func (u *Updater) Download(ctx context.Context) ([]byte, error) {
	asset := AssetName(u.GOOS, u.GOARCH)

	bin, status, err := u.get(ctx, u.assetURL(asset), "")
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", asset, err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("downloading %s: HTTP %d", asset, status)
	}

	shaBody, status, err := u.get(ctx, u.assetURL(asset+".sha256"), "")
	if err != nil {
		return nil, fmt.Errorf("downloading checksum for %s: %w", asset, err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("downloading checksum for %s: HTTP %d", asset, status)
	}

	expected, err := ParseChecksumFile(shaBody)
	if err != nil {
		return nil, fmt.Errorf("reading checksum for %s: %w", asset, err)
	}
	if err := VerifyChecksum(bin, expected); err != nil {
		return nil, err
	}
	return bin, nil
}
