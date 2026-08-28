// Package caddyedge drives a user-controlled Caddy edge node through its
// tailnet-only admin API to publish a local HTTP service at a custom public
// hostname (see kata v1z5, the `P` publish path). Caddy owns the public trust
// plane (custom-domain DNS, :443 ingress, TLS termination and renewal); this
// package only computes the route JSON and talks to Caddy's admin API over the
// tailnet.
//
// Two halves live here. The pure builders (IDFor, ValidHostname, BuildRoute)
// construct and identify routes with no I/O. The Client mutates the shared
// Caddy config; every field it needs from the outside world (the HTTP client,
// the admin URL, the server name) is a struct field, so tests drive the whole
// flow against an httptest.Server and never bind a real port — the same
// injectable shape as internal/selfupdate. Everything is pure Go on the
// standard library (net/http, encoding/json, ...), per the project's
// zero-non-Go-runtime-deps rule.
//
// Two invariants are structural, not merely tested:
//
//   - The upstream to the backend is ALWAYS plain http. There is no scheme
//     parameter anywhere in this package's public API, so an https-to-backend
//     route is unrepresentable — the emitted upstream is a bare `dial
//     "<label>:<port>"` with no transport.tls. Tailscale's WireGuard tunnel
//     already encrypts the hop; app-layer TLS on it would add nothing.
//   - The upstream Host header is rewritten to "<label>:<port>", never the
//     public hostname. Tailscale Serve routes on Host and 404s a public
//     hostname; this rewrite is a hard functional requirement (verified live
//     during planning), not a privacy nicety.
//
// The exact route JSON below is Caddy's documented admin-API schema. It is
// exercised here only against in-process httptest fakes; a real `caddy` binary
// confirming the shape is an opt-in, CI-only integration test that is NOT in
// this package's scope (see v1z5's Verification section).
package caddyedge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// idPrefix namespaces every route tailport owns. A route's @id is
// idPrefix+hostname, so ownership is decidable from the id alone and the id is
// deterministic from the public hostname.
const idPrefix = "tailport-"

// defaultTimeout bounds a single admin-API round trip when the caller injects
// no HTTPClient, matching internal/selfupdate's injectable default.
const defaultTimeout = 5 * time.Second

// maxBody caps any admin-API response read so a broken or hostile edge can't
// make tailport allocate unbounded memory. Admin responses (a route, the routes
// array) are kilobytes; 8 MiB is comfortably above that.
const maxBody = 8 << 20

// maxRetries bounds the optimistic-concurrency retry loop: because several
// tailport computers share one Caddy routes array, a read-then-mutate flow can
// lose an If-Match race (HTTP 412) and must re-read. After this many losses we
// give up with ErrConcurrentUpdate rather than spin forever.
const maxRetries = 3

// Sentinel errors let callers map failure modes onto specific messages with
// errors.Is instead of matching on strings.
var (
	// ErrUnreachable wraps any transport-level failure talking to the admin
	// API (connection refused, timeout, DNS). The edge could not be reached.
	ErrUnreachable = errors.New("caddy admin API unreachable")
	// ErrHostnameConflict means the public hostname is already claimed by a
	// different backend or a foreign (non-tailport) route. Public-hostname
	// takeover is never implicit: when this fires, NOTHING was mutated.
	ErrHostnameConflict = errors.New("public hostname already published elsewhere")
	// ErrNotFound means the route being unpublished does not exist.
	ErrNotFound = errors.New("route not found")
	// ErrConcurrentUpdate means the shared config kept moving under us: the
	// If-Match retry budget was exhausted without a clean mutation.
	ErrConcurrentUpdate = errors.New("caddy config changed concurrently; retries exhausted")
	// ErrNoConflict means PurgeConflict re-read the routes array and found nothing
	// overlapping the hostname any more: the conflict cleared between classify and
	// purge, so there is nothing to delete. The caller may retry the plain publish.
	ErrNoConflict = errors.New("no route overlaps the hostname; nothing to purge")
	// ErrConflictChanged means the route PurgeConflict re-located no longer matches
	// the identity classified at confirm time (an owned→foreign escalation, an owned
	// backend swap, or a foreign route that changed under us). The caller must
	// re-classify and re-confirm with the correct (possibly scarier) ladder rather
	// than delete a route approved under a now-stale, weaker confirm.
	ErrConflictChanged = errors.New("the conflicting route changed since it was classified")
	// ErrEdgeNoIfMatch means the edge returned no ETag on the read guarding a
	// force-delete, so the If-Match optimistic-concurrency guard the delete relies
	// on is absent (Caddy first shipped ETag/If-Match in 2.5.2; an older edge
	// IGNORES If-Match, turning every conditional delete into an UNCONDITIONAL one
	// that a concurrent array shift could point at the wrong route). PurgeConflict
	// refuses to force-delete rather than clobber blind. See docs/caddy-edge.md.
	ErrEdgeNoIfMatch = errors.New("edge did not return an ETag; refusing to force-delete — requires Caddy >= 2.5.2")
	// ErrRestoreNameClaimed means RestoreRoute's scan-before-append found the
	// hostname already re-claimed — an overlapping route, or a route carrying the
	// captured route's @id — so appending the captured bytes would create DUAL
	// exposure (Caddy permits overlapping host matchers) or a duplicate @id. The
	// undo (kata ttfh) refuses rather than dual-expose; our own take-over was
	// already removed by step A, so the honest outcome is "another route now
	// claims the host — resolve the drift in Caddy".
	ErrRestoreNameClaimed = errors.New("hostname re-claimed by another route; refusing to restore")
)

// BasicAuth is a single http_basic credential for the edge. Hash is a bcrypt
// MCF string (e.g. "$2a$..."), never plaintext — it arrives pre-hashed; this
// package never computes it. A nil *BasicAuth means "no auth on this route".
type BasicAuth struct {
	User string
	Hash string
}

// Route is the JSON shape of one Caddy admin-API route object. It is modeled as
// typed structs (cleaner than map[string]any) and marshals to Caddy's route
// schema, with @id serialized as the literal key "@id". Notably absent from
// Upstream is any transport/tls field — that omission is what makes the backend
// hop plain http and unrepresentable as https.
type Route struct {
	ID       string    `json:"@id"`
	Match    []Match   `json:"match"`
	Handle   []Handler `json:"handle"`
	Terminal bool      `json:"terminal"`
}

// Match is one Caddy request-matcher object. Only the host matcher is used to
// BUILD routes, but a LIVE route's match block can carry any matcher keys
// (path, method, header, …). Decoding must NOT silently drop them: a route
// whose sole match block is {"host":[...],"path":[...]} is more than just our
// hostname, and treating it as ours would let Publish broaden it (dropping the
// path constraint) or Unpublish delete it. So on decode we retain the raw
// key/value pairs of the match object in raw; ownership checks (hostMatcherIs)
// then demand the shape be exactly {host} and nothing more, and re-marshaling a
// decoded foreign route preserves its other matchers verbatim.
//
// Host is the decoded value of the "host" key, kept as a typed field because
// callers (parseRoute, findHostConflict) reason over hostnames directly.
type Match struct {
	Host []string
	// raw is every key of the match object as decoded, so the key SET is
	// recoverable and a round-trip preserves foreign matchers. It is nil for a
	// Match constructed in-process (BuildRoute), which marshals as a plain
	// {"host":[...]} and is never fed back through hostMatcherIs.
	raw map[string]json.RawMessage
}

// UnmarshalJSON records the full set of matcher keys present on the wire (in
// raw) and extracts the host list, so an ownership check can insist the match
// object is EXACTLY {host} rather than silently ignoring extra matchers.
func (m *Match) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.raw = raw
	m.Host = nil
	if h, ok := raw["host"]; ok {
		if err := json.Unmarshal(h, &m.Host); err != nil {
			return err
		}
	}
	return nil
}

// MarshalJSON emits a plain {"host":[...]} matcher for an in-process Match (the
// shape BuildRoute must produce), and for a decoded Match re-emits every key it
// carried, so a foreign route's other matchers survive a round-trip untouched.
func (m Match) MarshalJSON() ([]byte, error) {
	if m.raw != nil {
		return json.Marshal(m.raw)
	}
	return json.Marshal(map[string][]string{"host": m.Host})
}

// Handler is one entry in a route's handle chain. It carries every field any of
// the handlers used here needs; the omitempty tags mean each serialized handler
// shows only its own keys. An "authentication" handler carries Providers; a
// "reverse_proxy" handler carries Upstreams and Headers.
type Handler struct {
	Handler   string        `json:"handler"`
	Providers *Providers    `json:"providers,omitempty"`
	Upstreams []Upstream    `json:"upstreams,omitempty"`
	Headers   *ProxyHeaders `json:"headers,omitempty"`
}

// Providers is the authentication handler's provider set (http_basic only).
type Providers struct {
	HTTPBasic HTTPBasic `json:"http_basic"`
}

// HTTPBasic is the http_basic provider config: a bcrypt hash algorithm plus the
// account list.
type HTTPBasic struct {
	Hash     HashConfig `json:"hash"`
	Accounts []Account  `json:"accounts"`
}

// HashConfig names the password hashing algorithm; always "bcrypt" here.
type HashConfig struct {
	Algorithm string `json:"algorithm"`
}

// Account is one http_basic username + bcrypt-hashed password.
type Account struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Upstream is a reverse_proxy dial target. It has ONLY a dial address: there is
// deliberately no transport field, so the backend hop can never be anything but
// plain http.
type Upstream struct {
	Dial string `json:"dial"`
}

// ProxyHeaders / HeaderOps model reverse_proxy's request header operations —
// here only the required upstream Host rewrite.
type ProxyHeaders struct {
	Request HeaderOps `json:"request"`
}

// HeaderOps is a set of header operations; only Set is used.
type HeaderOps struct {
	Set map[string][]string `json:"set"`
}

// RouteInfo is the parsed, flattened view of a live route that List returns.
// It is a separate type from the wire Route on purpose: callers reason about a
// route's public hostname, its backend, whether it carries auth, and whether
// tailport owns it — not the nested handler chain.
type RouteInfo struct {
	Hostname string // public host matcher, match[0].host[0]
	Label    string // backend short MagicDNS label, from the reverse_proxy dial
	Port     int    // backend port, from the reverse_proxy dial
	Auth     bool   // an authentication handler is present
	Owned    bool   // the @id carries the tailport- prefix
}

// ConflictKind classifies WHY a Publish returned ErrHostnameConflict, so the UI
// can name the current holder and refuse with a specific, actionable message
// (kata qfbf; design docs/caddy-edge-conflict-design.md §3.1). It is the output
// of InspectConflict, a pure read-only classifier.
type ConflictKind int

const (
	// None means nothing overlaps the requested hostname AND our @id is clean:
	// the conflict cleared between Publish's refusal and this read. The caller
	// may retry the plain publish ONCE (bounded — never a spin).
	None ConflictKind = iota
	// OwnedDiffBackend means a tailport-owned @id already holds the hostname but
	// points at a DIFFERENT backend/port than the one requested. The backend may
	// be this machine's (a publish from another local port) or another machine's
	// (detectable by comparing the backend label to this machine's short label).
	OwnedDiffBackend
	// IdHijacked means our deterministic @id still exists but its host matcher was
	// re-pointed by a foreign edit to a different, possibly NON-overlapping host.
	// A plain host-overlap scan for the requested name would miss this (the route
	// no longer matches our hostname) and the old "retry on no-overlap" rule would
	// livelock — Publish re-refuses on hostMatcherIs forever. So it is always a
	// refusal, never a retry, never a blind DELETE (that would delete drift).
	IdHijacked
	// ForeignOverlap means a route tailport did NOT create already claims a
	// hostname overlapping the request — including a non-reverse_proxy route
	// (static_response / file_server / …) that List would skip, since the scan
	// matches on the host matcher alone.
	ForeignOverlap
)

// String renders a ConflictKind for test output and diagnostics.
func (k ConflictKind) String() string {
	switch k {
	case None:
		return "None"
	case OwnedDiffBackend:
		return "OwnedDiffBackend"
	case IdHijacked:
		return "IdHijacked"
	case ForeignOverlap:
		return "ForeignOverlap"
	default:
		return "ConflictKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// ConflictInfo is InspectConflict's read-only classification of the route that
// currently holds a hostname a Publish refused. It names the holder so the UI
// can refuse specifically; it deliberately carries NO capture bytes, array
// index, or etag. Capture and the fresh-read purge are kata 6n15's job — doing
// its own byte-faithful capture — so keeping this a pure classifier avoids any
// dependency on raw-storage fidelity and keeps it lean.
type ConflictInfo struct {
	Kind ConflictKind // None | OwnedDiffBackend | IdHijacked | ForeignOverlap
	// Owned reports whether the holder's @id carries the tailport- prefix.
	Owned bool
	// ID is the holder's @id ("" for an id-less foreign route).
	ID string
	// Label/Port/BackendParseable describe the holder's reverse_proxy dial, when
	// it has one. BackendParseable is false for a route with no parseable
	// reverse_proxy backend (a non-proxy foreign route), where Handler names it.
	Label            string
	Port             int
	BackendParseable bool
	// HijackedTo, set only for IdHijacked, describes where our @id now points
	// (the host matcher(s) it was re-pointed at), so the UI can name it.
	HijackedTo string
	// Handler is the holder's first handler name (e.g. "reverse_proxy",
	// "static_response"), used to name a non-proxy foreign route the backend
	// dial can't.
	Handler string
	// Hosts is the holder's FULL host-matcher list (every host pattern across
	// every match block), populated for a ForeignOverlap so the UI can disclose
	// the true blast radius of a force-purge (kata 6n15 / roborev hped #3): a
	// foreign route matching a bare "*" catch-all, a "*.suffix" wildcard, or
	// several hostnames serves more than the one requested, and the confirm must
	// name every pattern so the user isn't deleting unrelated public hostnames
	// blind. Empty for a route with no host matcher.
	Hosts []string
	// RawHash is an opaque fingerprint of the LOCATED overlapping route's exact
	// element bytes, set for a ForeignOverlap so PurgeConflict can re-locate that
	// PRECISE element later rather than the first overlap (roborev ve95 FIX 1).
	// It closes a hole for an id-less foreign route: with no @id to pin it, a
	// structurally-similar sibling inserted ahead of the approved route between
	// classify and purge would otherwise be the first overlap PurgeConflict finds
	// and deletes. ExpectFromConflict carries it into PurgeExpect.RawHash for the
	// id-less case (ID==""), where it becomes the located route's identity. Empty
	// when nothing overlaps (Kind==None). The hash is read over the same raw
	// element bytes fetchRoutesRaw yields, so it is byte-stable across the two
	// reads as long as the route itself has not changed.
	RawHash string
}

// canonHost canonicalizes a public hostname for identity and comparison. DNS
// hostnames are case-insensitive, so lowercasing (after trimming surrounding
// whitespace) is the single boundary transform that keeps casing from ever
// minting a distinct-but-overlapping route or a phantom-distinct @id. Every
// entry point that identifies or matches on a hostname runs through it.
func canonHost(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// IDFor returns the deterministic @id tailport uses for a route publishing the
// given public hostname. The hostname is canonicalized first, so case-variant
// spellings of one hostname collapse to a single @id (Caddy treats them as one
// route; distinct @ids would be phantom-distinct).
func IDFor(hostname string) string {
	return idPrefix + canonHost(hostname)
}

// ValidHostname reports whether s is a syntactically valid DNS hostname usable
// as a public route host. Dots ARE allowed so nested subdomains like
// "a.b.example.com" pass. It rejects the empty string, whitespace, schemes and
// paths ("http://x", "x/y"), ports ("x:8080"), leading/trailing or doubled
// dots, and any label that isn't a valid LDH (letter/digit/hyphen) label.
func ValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	// A scheme, a port, a path, or any whitespace all disqualify outright.
	if strings.ContainsAny(s, ":/ \t\r\n") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

// validLabel reports whether one dot-separated component is a valid DNS label.
// An empty label (from a leading/trailing/doubled dot) fails here.
func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		ch := label[i]
		alnum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
		if !alnum && ch != '-' {
			return false
		}
	}
	return true
}

// BuildRoute constructs the Caddy route object that publishes public hostname
// to the plain-http tailnet backend "<label>:<port>". auth==nil means no
// authentication handler; when non-nil, the authentication handler is emitted
// BEFORE the reverse_proxy handler in the handle chain (order is significant —
// auth must run first). There is intentionally no scheme parameter: the emitted
// upstream is always plain http.
func BuildRoute(hostname, label string, port int, auth *BasicAuth) Route {
	hostname = canonHost(hostname)
	dial := fmt.Sprintf("%s:%d", label, port)

	var handle []Handler
	if auth != nil {
		handle = append(handle, Handler{
			Handler: "authentication",
			Providers: &Providers{
				HTTPBasic: HTTPBasic{
					Hash: HashConfig{Algorithm: "bcrypt"},
					Accounts: []Account{{
						Username: auth.User,
						Password: auth.Hash, // pre-hashed bcrypt MCF, never plaintext
					}},
				},
			},
		})
	}
	handle = append(handle, Handler{
		Handler:   "reverse_proxy",
		Upstreams: []Upstream{{Dial: dial}},
		// header_up Host {label}:{port}: Tailscale Serve routes on Host and
		// would 404 the public hostname. Functional requirement, not a nicety.
		Headers: &ProxyHeaders{
			Request: HeaderOps{Set: map[string][]string{"Host": {dial}}},
		},
	})

	return Route{
		ID:       IDFor(hostname),
		Match:    []Match{{Host: []string{hostname}}},
		Handle:   handle,
		Terminal: true,
	}
}

// Client talks to a Caddy edge's admin API. Every external dependency is a
// field so tests inject an httptest.Server: HTTPClient is the transport (nil
// yields a defaultTimeout client), AdminURL is the tailnet-only admin base
// (e.g. http://caddy:2019), and ServerName selects the shared HTTP server whose
// routes array all tailport computers publishing through this edge write into.
type Client struct {
	HTTPClient *http.Client // injectable; nil -> a defaultTimeout default
	AdminURL   string       // e.g. http://caddy:2019
	ServerName string       // caddy.server_name, e.g. "tailport"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (c *Client) idURL(id string) string {
	return strings.TrimRight(c.AdminURL, "/") + "/id/" + id
}

func (c *Client) routesURL() string {
	return strings.TrimRight(c.AdminURL, "/") + "/config/apps/http/servers/" + c.ServerName + "/routes"
}

// do performs one admin-API request. It sets If-Match when ifMatch is non-empty
// and Content-Type on a body, reads a bounded response, and returns the body,
// status, and the response Etag. Any transport error maps to ErrUnreachable.
func (c *Client) do(ctx context.Context, method, url string, body []byte, ifMatch string) (respBody []byte, status int, etag string, err error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		// A body read that fails after headers arrive (client timeout, dropped
		// connection mid-body) is still a transport failure: the edge could not
		// be read to completion. Honor the sentinel contract and wrap it.
		return nil, resp.StatusCode, "", fmt.Errorf("%w: reading caddy response: %v", ErrUnreachable, err)
	}
	return b, resp.StatusCode, resp.Header.Get("Etag"), nil
}

// statusError wraps a non-2xx admin response that isn't a modeled case,
// carrying Caddy's own response body text for diagnostics.
func (c *Client) statusError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("caddy admin API returned HTTP %d", status)
	}
	return fmt.Errorf("caddy admin API returned HTTP %d: %s", status, msg)
}

// fetchByID GETs /id/<id>. found is false on 404 (the route is absent).
func (c *Client) fetchByID(ctx context.Context, id string) (route Route, etag string, found bool, err error) {
	body, status, etag, err := c.do(ctx, http.MethodGet, c.idURL(id), nil, "")
	if err != nil {
		return Route{}, "", false, err
	}
	if status == http.StatusNotFound {
		return Route{}, "", false, nil
	}
	if status < 200 || status >= 300 {
		return Route{}, "", false, c.statusError(status, body)
	}
	if err := json.Unmarshal(body, &route); err != nil {
		return Route{}, "", false, fmt.Errorf("parsing caddy route: %w", err)
	}
	return route, etag, true, nil
}

// fetchByIDRaw GETs /id/<id> as RAW JSON bytes plus the id-scope Etag. It is the
// raw-fidelity twin of fetchByID, used by PurgeConflict's /id delete path so the
// re-verify and the capture both run over the FRESH id-read bytes (roborev hped
// #1) — not the stale array bytes read moments earlier. The returned raw is a
// copy the caller owns (byte-faithful even for fields tailport doesn't model);
// found is false on 404 (the route vanished between the array read and now).
func (c *Client) fetchByIDRaw(ctx context.Context, id string) (raw json.RawMessage, etag string, found bool, err error) {
	body, status, etag, err := c.do(ctx, http.MethodGet, c.idURL(id), nil, "")
	if err != nil {
		return nil, "", false, err
	}
	if status == http.StatusNotFound {
		return nil, "", false, nil
	}
	if status < 200 || status >= 300 {
		return nil, "", false, c.statusError(status, body)
	}
	// Unmarshal into a RawMessage to trim any transport framing (e.g. an
	// encoder's trailing newline) while preserving the element's exact value
	// bytes — the same normalization fetchRoutesRaw's elements get, so a capture
	// taken here is byte-comparable to one taken from the array read.
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", false, fmt.Errorf("parsing caddy route: %w", err)
	}
	return raw, etag, true, nil
}

// fetchRoutes GETs the shared routes array and returns it with the array's Etag
// (for If-Match on a subsequent mutation). A 404 or null body is treated as an
// empty array, not an error.
func (c *Client) fetchRoutes(ctx context.Context) (routes []Route, etag string, err error) {
	body, status, etag, err := c.do(ctx, http.MethodGet, c.routesURL(), nil, "")
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusNotFound {
		return nil, etag, nil
	}
	if status < 200 || status >= 300 {
		return nil, "", c.statusError(status, body)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, etag, nil
	}
	if err := json.Unmarshal(body, &routes); err != nil {
		return nil, "", fmt.Errorf("parsing caddy routes: %w", err)
	}
	return routes, etag, nil
}

// mutate marshals route and sends it via method (POST/PATCH) with the given
// If-Match etag, returning the status and body. A transport error is already
// mapped to ErrUnreachable by do.
func (c *Client) mutate(ctx context.Context, method, url string, route Route, etag string) (status int, body []byte, err error) {
	payload, err := json.Marshal(route)
	if err != nil {
		return 0, nil, fmt.Errorf("encoding route: %w", err)
	}
	b, status, _, err := c.do(ctx, method, url, payload, etag)
	if err != nil {
		return 0, nil, err
	}
	return status, b, nil
}

// List returns the parsed view of every route on the shared server. Routes that
// can't be parsed (no host matcher, no reverse_proxy dial, unparseable
// backend) are skipped, not fatal.
func (c *Client) List(ctx context.Context) ([]RouteInfo, error) {
	routes, _, err := c.fetchRoutes(ctx)
	if err != nil {
		return nil, err
	}
	var out []RouteInfo
	for _, r := range routes {
		if info, ok := parseRoute(r); ok {
			out = append(out, info)
		}
	}
	return out, nil
}

// Publish publishes hostname to the plain-http tailnet backend label:port,
// optionally behind auth. It never takes over a hostname it does not already
// own for this exact backend:
//
//   - Present (a tailport route already owns this @id) with the SAME backend
//     label+port: an idempotent republish via PATCH /id (never PUT).
//   - Present with a DIFFERENT backend or port: ErrHostnameConflict naming the
//     current backend, no mutation.
//   - Absent, but a foreign route already matches the hostname:
//     ErrHostnameConflict, no mutation.
//   - Absent and unclaimed: POST the new route onto the shared array.
//
// Every mutation carries an If-Match Etag; an HTTP 412 re-reads and re-evaluates
// (bounded by maxRetries, then ErrConcurrentUpdate).
func (c *Client) Publish(ctx context.Context, hostname, label string, port int, auth *BasicAuth) error {
	hostname = canonHost(hostname)
	id := IDFor(hostname)
	for attempt := 0; attempt < maxRetries; attempt++ {
		route, etag, found, err := c.fetchByID(ctx, id)
		if err != nil {
			return err
		}
		if found {
			// The @id is ours by construction, but a foreign edit could have
			// kept the @id while changing or adding host matchers. Trusting the
			// @id alone would let us overwrite an unrelated public route, so
			// require the live matcher to still be exactly this one hostname
			// before mutating; otherwise refuse without touching anything.
			if !hostMatcherIs(route, hostname) {
				return fmt.Errorf("%w: route %q no longer matches only %q; refusing to overwrite",
					ErrHostnameConflict, id, hostname)
			}
			curLabel, curPort, ok := backendOf(route)
			if !ok {
				return fmt.Errorf("existing route %q has an unparseable backend", id)
			}
			if curLabel != label || curPort != port {
				return fmt.Errorf("%w: %q is published to %s:%d, not %s:%d",
					ErrHostnameConflict, hostname, curLabel, curPort, label, port)
			}
			status, body, err := c.mutate(ctx, http.MethodPatch, c.idURL(id), BuildRoute(hostname, label, port, auth), etag)
			if err != nil {
				return err
			}
			if status == http.StatusPreconditionFailed {
				continue // config moved under us; re-read and re-evaluate
			}
			if status < 200 || status >= 300 {
				return c.statusError(status, body)
			}
			return nil
		}

		// Absent: scan the shared array for anyone already claiming this
		// hostname. Any match here is foreign — a tailport-owned match would
		// have been found by @id above.
		routes, arrEtag, err := c.fetchRoutes(ctx)
		if err != nil {
			return err
		}
		if who := findHostConflict(routes, hostname); who != "" {
			return fmt.Errorf("%w: %q is already claimed by %s", ErrHostnameConflict, hostname, who)
		}
		status, body, err := c.mutate(ctx, http.MethodPost, c.routesURL(), BuildRoute(hostname, label, port, auth), arrEtag)
		if err != nil {
			return err
		}
		if status == http.StatusPreconditionFailed {
			continue // lost the create race; re-read, re-scan, retry
		}
		if status < 200 || status >= 300 {
			return c.statusError(status, body)
		}
		return nil
	}
	return ErrConcurrentUpdate
}

// Unpublish removes the route for hostname, but only after re-verifying live
// that it still belongs to tailport AND still points at the given label:port.
// On stale or mismatched state it refuses rather than deleting blind:
// ErrNotFound when absent, ErrHostnameConflict when it now points elsewhere. A
// 404 during the DELETE itself is success (someone already removed it).
func (c *Client) Unpublish(ctx context.Context, hostname, label string, port int) error {
	hostname = canonHost(hostname)
	id := IDFor(hostname)
	for attempt := 0; attempt < maxRetries; attempt++ {
		route, etag, found, err := c.fetchByID(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		// A foreign edit could have kept our @id while changing or adding host
		// matchers. Deleting on @id alone would then remove an unrelated public
		// route, so require the live matcher to still be exactly this one
		// hostname before deleting; otherwise refuse.
		if !hostMatcherIs(route, hostname) {
			return fmt.Errorf("%w: route %q no longer matches only %q; refusing to delete",
				ErrHostnameConflict, id, hostname)
		}
		curLabel, curPort, ok := backendOf(route)
		if !ok {
			return fmt.Errorf("route %q has an unparseable backend; refusing to delete", id)
		}
		if curLabel != label || curPort != port {
			return fmt.Errorf("%w: %q now points to %s:%d, not %s:%d; refusing to delete",
				ErrHostnameConflict, hostname, curLabel, curPort, label, port)
		}
		body, status, _, err := c.do(ctx, http.MethodDelete, c.idURL(id), nil, etag)
		if err != nil {
			return err
		}
		if status == http.StatusPreconditionFailed {
			continue // config moved under us; re-verify and retry
		}
		if status == http.StatusNotFound {
			return nil // already gone: success
		}
		if status < 200 || status >= 300 {
			return c.statusError(status, body)
		}
		return nil
	}
	return ErrConcurrentUpdate
}

// InspectConflict classifies why a publish of hostname collided, WITHOUT
// mutating anything (kata qfbf; design §3.1). The UI calls it after Publish
// returns ErrHostnameConflict, to name the holder and (in a later pillar) branch
// to force-purge. wantLabel/wantPort (kata vsx4 #2) name the backend the CALLER
// is trying to publish -- InspectConflict doesn't otherwise know this, and a
// route that changed to point at exactly this backend between the failed
// Publish and this read is no longer a conflict at all. It does TWO reads:
//
//  1. fetchByID(IDFor(hostname)) — if our @id exists but its matcher is NOT
//     exactly our hostname, a foreign edit re-pointed it (possibly to a
//     non-overlapping host). A plain overlap scan for the requested name would
//     MISS this and the old "retry on no overlap" rule would livelock, so this
//     is classified IdHijacked (carrying where it now points) and refused, never
//     retried, never blindly deleted. If the @id exists and its matcher IS ours,
//     Publish refused only because of the backend: if the live backend now
//     equals wantLabel:wantPort, the route already matches what we'd publish —
//     Kind=None lets the caller's bounded retry PATCH it (applying the caller's
//     config, e.g. auth) instead of falsely refusing. Otherwise OwnedDiffBackend.
//  2. a raw host-overlap scan of the shared routes array (reusing hostsOverlap)
//     for the foreign-overlap case. This walks the raw routes and matches on the
//     host matcher ALONE, so it SEES routes List would drop — a foreign
//     static_response/file_server with no reverse_proxy dial → ForeignOverlap.
//
// If our @id is clean and nothing overlaps, Kind==None: the conflict cleared and
// the caller may retry the plain publish once.
func (c *Client) InspectConflict(ctx context.Context, hostname, wantLabel string, wantPort int) (ConflictInfo, error) {
	hostname = canonHost(hostname)
	id := IDFor(hostname)

	// Read 1: our own @id.
	route, _, found, err := c.fetchByID(ctx, id)
	if err != nil {
		return ConflictInfo{}, err
	}
	if found {
		if !hostMatcherIs(route, hostname) {
			// Hijacked: our @id kept but its matcher re-pointed elsewhere.
			info := ConflictInfo{Kind: IdHijacked, Owned: true, ID: route.ID}
			info.HijackedTo = matcherHosts(route)
			info.Handler = firstHandler(route)
			if label, port, ok := backendOf(route); ok {
				info.Label, info.Port, info.BackendParseable = label, port, true
			}
			return info, nil
		}
		// Our @id, matcher still exactly our hostname. Publish only refuses such a
		// route for a different backend/port, so this is OwnedDiffBackend — UNLESS
		// the live backend already equals the one the caller wants to publish, in
		// which case the conflict has resolved itself (e.g. another retry of ours,
		// or a concurrent identical publish, already won) and there is nothing left
		// to refuse: Kind=None so the caller retries and PATCHes it. (A backend that
		// actually matched at Publish-time would have PATCHed cleanly there; an
		// unparseable backend surfaces as BackendParseable==false but is still owned.)
		if label, port, ok := backendOf(route); ok && label == wantLabel && port == wantPort {
			// Our route already points where we want, so there's nothing to refuse
			// about IT — but before declaring the conflict resolved, scan the shared
			// array for ANOTHER route overlapping the hostname (roborev 2g50): a
			// coexisting foreign exact/wildcard route would still intercept traffic
			// or bypass the requested auth, and a bare Kind=None here would let the
			// caller PATCH-and-"succeed" blind to it. Exclude our own @id so we don't
			// rediscover the route we just cleared.
			return c.scanOverlap(ctx, hostname, route.ID)
		}
		info := ConflictInfo{Kind: OwnedDiffBackend, Owned: true, ID: route.ID, Handler: firstHandler(route)}
		if label, port, ok := backendOf(route); ok {
			info.Label, info.Port, info.BackendParseable = label, port, true
		}
		return info, nil
	}

	// Read 2: nothing owns our @id — scan the shared array for a foreign overlap.
	return c.scanOverlap(ctx, hostname, "")
}

// scanOverlap walks the shared routes array for a route whose host matcher
// overlaps hostname, skipping the route whose @id == excludeID (so a caller that
// has already accounted for its own owned route doesn't rediscover it). It
// matches on the host matcher ALONE, so a foreign non-reverse_proxy route that
// List/parseRoute would drop is still found. The first overlap is returned as
// ForeignOverlap (carrying its full host list for the blast-radius disclosure,
// roborev hped #3); no overlap → Kind=None.
//
// It reads the array RAW (fetchRoutesRaw) so it can fingerprint the LOCATED
// overlapping element's exact bytes into ConflictInfo.RawHash (roborev ve95 FIX
// 1): the same normalization PurgeConflict's own fetchRoutesRaw applies, so the
// hash re-locates the identical element at purge time even for an id-less foreign
// route that carries no @id to pin it by.
func (c *Client) scanOverlap(ctx context.Context, hostname, excludeID string) (ConflictInfo, error) {
	raws, _, err := c.fetchRoutesRaw(ctx)
	if err != nil {
		return ConflictInfo{}, err
	}
	for _, elem := range raws {
		var r Route
		if json.Unmarshal(elem, &r) != nil {
			continue // an element we can't decode can't be matched; skip it
		}
		if excludeID != "" && r.ID == excludeID {
			continue
		}
		if !routeOverlaps(r, hostname) {
			continue
		}
		info := ConflictInfo{
			Kind:    ForeignOverlap,
			Owned:   strings.HasPrefix(r.ID, idPrefix),
			ID:      r.ID,
			Handler: firstHandler(r),
			Hosts:   matcherHostList(r),
			RawHash: rawIdentityHash(elem),
		}
		if label, port, ok := backendOf(r); ok {
			info.Label, info.Port, info.BackendParseable = label, port, true
		}
		return info, nil
	}
	return ConflictInfo{Kind: None}, nil
}

// Captured is the byte-faithful record of a route PurgeConflict deleted, so a
// later undo (kata ttfh) can re-POST the EXACT bytes Caddy held rather than a
// lossy re-serialization (design §3.3 — Route/Handler have no raw catch-all, so
// a decode+re-marshal would silently drop any field tailport doesn't model). Raw
// is meaningful for undo only when the purge was OWNED (design OQ8); it is
// returned for every purge (harmless) and the caller decides whether to arm undo.
type Captured struct {
	// Raw is the exact JSON element bytes of the deleted route.
	Raw json.RawMessage
	// Hostname is the public hostname whose conflict was purged (canonicalized).
	Hostname string
	// HadID reports whether the deleted route carried an @id (so it was deleted by
	// /id/<id>, and an owned capture is undoable by re-POST); false for a truly
	// id-less foreign route deleted by array index.
	HadID bool
}

// PurgeExpect is the identity of the conflicting route as classified at confirm
// time. PurgeConflict re-verifies the live route against it just before deleting,
// so a route the user approved under one confirm can never be silently deleted
// after it changed underneath them (design §3.2, review r1-F8). The load-bearing
// safety property is Owned: a route approved as OWNED (the normal confirm) that
// has since become FOREIGN fails this check → ErrConflictChanged → the UI
// re-classifies and re-opens the scarier ladder rather than deleting drift under
// a weaker confirm.
//
// It is built from a ConflictInfo (ExpectFromConflict). Because ConflictInfo
// carries no raw bytes by design (§3.1), the default identity is STRUCTURAL —
// owned-ness, @id, and the backend (label:port for a proxy, else the handler
// name). A caller that has the raw element bytes may instead pin RawHash for an
// exact-bytes identity of an id-less foreign route; when RawHash is set it, plus
// owned-ness, is the whole check.
type PurgeExpect struct {
	Owned            bool
	ID               string
	BackendParseable bool
	Label            string
	Port             int
	Handler          string
	// RawHash, when non-empty, replaces the structural backend/@id check with an
	// exact hash of the element's raw bytes (design §3.2, for an id-less foreign
	// route). Owned-ness is still checked alongside it.
	RawHash string
}

// ExpectFromConflict builds the re-verify identity from a read-only conflict
// classification (the identity the UI showed the user at confirm time). It uses
// the structural fields ConflictInfo carries, plus the confirmed host set (Hosts)
// so a foreign matcher widen is refused (roborev ve95 FIX 2). For an id-less
// foreign route (ID=="") it also pins RawHash to the located element's exact
// bytes (roborev ve95 FIX 1): with no @id, RawHash is the only stable identity
// that re-locates the PRECISE approved route rather than the first overlap. A
// route WITH an @id keeps the structural (@id + backend) identity, so a benign
// byte change to it doesn't spuriously re-open the ladder; its widen is caught by
// the Hosts pin instead.
func ExpectFromConflict(info ConflictInfo) PurgeExpect {
	e := PurgeExpect{
		Owned:            info.Owned,
		ID:               info.ID,
		BackendParseable: info.BackendParseable,
		Label:            info.Label,
		Port:             info.Port,
		Handler:          info.Handler,
	}
	// Pin the EXACT bytes for EVERY foreign route (roborev cmr5-#1), not just the
	// id-less one. A foreign @id route can keep the same flattened host list while
	// widening what it matches -- adding a hostless OR match block, or dropping a
	// path/method constraint -- so a host-SET check (the earlier ve95 approach) is
	// insufficient. RawHash re-verifies the whole element, so ANY change to what
	// the route matches → re-classify + re-disclose. For the scary force-delete
	// path this is the correct, safe pin; a foreign route's stored bytes don't
	// reformat on their own, so spurious re-confirms are unrealistic. Owned routes
	// carry no RawHash: they stay on @id+backend identity and are pinned to exactly
	// their hostname by hostMatcherIs.
	if !info.Owned {
		e.RawHash = info.RawHash
	}
	return e
}

// matches reports whether the live route (decoded r, raw bytes raw) still has the
// classified identity. owned-ness is always required to be stable (the escalation
// guard); then either the RawHash exact-bytes identity or the structural
// @id+backend identity must hold.
func (e PurgeExpect) matches(r Route, raw json.RawMessage) bool {
	if strings.HasPrefix(r.ID, idPrefix) != e.Owned {
		return false // owned↔foreign escalation: never delete under a stale confirm
	}
	if e.RawHash != "" {
		// Exact-bytes identity for EVERY foreign route (roborev cmr5-#1, superseding
		// the earlier host-SET pin): the raw hash fixes the WHOLE element -- every
		// match block and constraint -- so a foreign route that widened what it
		// matches while keeping its flattened host list (a hostless OR block, a
		// dropped path constraint) fails here and is re-classified rather than
		// deleted with a blast radius the user never saw. Owned routes carry no
		// RawHash and stay on @id+backend below, pinned to their host by hostMatcherIs.
		return raw != nil && rawIdentityHash(raw) == e.RawHash
	}
	if r.ID != e.ID {
		return false
	}
	label, port, ok := backendOf(r)
	if ok != e.BackendParseable {
		return false
	}
	if ok {
		return label == e.Label && port == e.Port
	}
	// A non-proxy route the backend dial can't name: pin the handler instead.
	return firstHandler(r) == e.Handler
}

// PurgeConflict force-deletes the route currently holding hostname and returns
// its captured bytes, so the caller can take the hostname over (design §3.2). It
// is the destructive counterpart to InspectConflict and is only ever reached
// behind the UI's escalated confirm ladders.
//
// Each attempt (≤ maxRetries) re-reads the shared routes array with a FRESH
// path-scoped etag and re-locates the route the user actually approved BY ITS
// IDENTITY, not by first host-overlap (roborev hped #4): by @id when expect.ID
// is set, else by exact-bytes hash when expect.RawHash is set, else (a foreign
// route classified with neither) by first overlap. Locating by identity means a
// foreign route that merely PRECEDES the approved owned route in the array can no
// longer be re-located and rejected in an endless re-classify loop. If the
// expected route is gone but something still overlaps → ErrConflictChanged (the
// holder changed — re-classify); if nothing overlaps at all → ErrNoConflict.
//
// It then re-verifies the located route still matches expect and deletes:
//
//   - By /id/<id> when the route carries an @id. Here it re-reads the route as
//     RAW bytes under its own id-scope etag (roborev hped #1): the re-verify and
//     the CAPTURE both run over those FRESH id-read bytes, and — because an owned
//     route's matcher is always exactly its hostname — the fresh matcher must
//     still be ours (hostMatcherIs) for an owned route, or at least still overlap
//     for a foreign one. An @id re-pointed to a different (or widened) host
//     between the array read and the id read thus fails here → ErrConflictChanged,
//     nothing deleted; and the captured bytes are the ones actually deleted.
//   - By DELETE .../routes/<index> under the routes-array etag for a truly
//     id-less foreign route; the parent-scope If-Match re-hashes the whole array,
//     so any concurrent add/remove/reorder → 412 → re-read.
//
// If the guarding ETag is absent (a pre-2.5.2 edge that ignores If-Match, turning
// the delete unconditional) it REFUSES with ErrEdgeNoIfMatch rather than clobber
// blind (roborev hped #6). A 412 or a vanished route re-reads and retries; the
// retry budget exhausting yields ErrConcurrentUpdate.
func (c *Client) PurgeConflict(ctx context.Context, hostname string, expect PurgeExpect) (Captured, error) {
	hostname = canonHost(hostname)
	for attempt := 0; attempt < maxRetries; attempt++ {
		raws, arrEtag, err := c.fetchRoutesRaw(ctx)
		if err != nil {
			return Captured{}, err
		}

		// Locate the approved route BY IDENTITY (roborev hped #4), not first
		// overlap: an @id-identified route by its @id, an id-less foreign route
		// pinned by RawHash by that hash, else (no stable identity carried) by first
		// host-overlap.
		idx := locateByExpect(raws, hostname, expect)
		if idx < 0 {
			// The approved route is gone. If something still overlaps, the holder
			// changed under us → re-classify; else there is nothing left to purge.
			if rawsOverlap(raws, hostname) {
				return Captured{}, ErrConflictChanged
			}
			return Captured{}, ErrNoConflict
		}
		raw := raws[idx]
		var route Route
		if err := json.Unmarshal(raw, &route); err != nil {
			// We located it by @id/hash but can no longer decode it: treat as drifted.
			return Captured{}, ErrConflictChanged
		}
		if !expect.matches(route, raw) {
			return Captured{}, ErrConflictChanged
		}

		var body []byte
		var status int
		var captured Captured
		if route.ID != "" {
			// Prefer stable @id deletion. Re-read the route as RAW bytes under its
			// own id-scope etag so the re-verify and the capture use the FRESH bytes
			// (roborev hped #1). A 404 means it vanished between reads → re-read.
			freshRaw, idEtag, found, err := c.fetchByIDRaw(ctx, route.ID)
			if err != nil {
				return Captured{}, err
			}
			if !found {
				continue
			}
			var live Route
			if err := json.Unmarshal(freshRaw, &live); err != nil {
				return Captured{}, ErrConflictChanged
			}
			// The id-read must STILL be the approved route. Identity must match, and
			// the matcher must not have drifted off the hostname: an OWNED route must
			// still be EXACTLY our hostname (a re-point or a widen-to-wildcard is
			// drift we must never delete under the owned confirm — roborev hped #1);
			// a foreign route (which may legitimately carry a wildcard/multi-host
			// matcher) must at least still overlap the hostname.
			matcherOK := routeOverlaps(live, hostname)
			if expect.Owned {
				matcherOK = hostMatcherIs(live, hostname)
			}
			if !matcherOK || !expect.matches(live, freshRaw) {
				return Captured{}, ErrConflictChanged
			}
			// Refuse a destructive delete with no guarding ETag (roborev hped #6):
			// on a pre-2.5.2 edge If-Match is ignored, so this would be an
			// UNCONDITIONAL delete a concurrent shift could point at the wrong route.
			if idEtag == "" {
				return Captured{}, ErrEdgeNoIfMatch
			}
			// Capture the FRESH id-read bytes — the exact bytes being deleted, and
			// byte-faithful even for fields tailport doesn't model.
			captured = Captured{
				Raw:      append(json.RawMessage(nil), freshRaw...),
				Hostname: hostname,
				HadID:    true,
			}
			body, status, _, err = c.do(ctx, http.MethodDelete, c.idURL(route.ID), nil, idEtag)
			if err != nil {
				return Captured{}, err
			}
		} else {
			// Truly id-less foreign route: delete by array index under the
			// routes-array etag. The parent-scope If-Match re-hashes the whole
			// array, so an index-shifting concurrent edit → 412 → re-read.
			if arrEtag == "" {
				return Captured{}, ErrEdgeNoIfMatch // roborev hped #6
			}
			captured = Captured{
				Raw:      append(json.RawMessage(nil), raw...),
				Hostname: hostname,
				HadID:    false,
			}
			body, status, _, err = c.do(ctx, http.MethodDelete, c.routesURL()+"/"+strconv.Itoa(idx), nil, arrEtag)
			if err != nil {
				return Captured{}, err
			}
		}
		if status == http.StatusPreconditionFailed {
			continue // config moved under us; re-read, re-verify, retry
		}
		if status == http.StatusNotFound {
			continue // already gone; re-read (a subsequent no-overlap → ErrNoConflict)
		}
		if status < 200 || status >= 300 {
			return Captured{}, c.statusError(status, body)
		}
		return captured, nil
	}
	return Captured{}, ErrConcurrentUpdate
}

// RestoreRoute re-creates a route from its captured bytes, as undo step B (kata
// ttfh; design §3.6-B). It is scan-then-append under ONE routes-array If-Match:
// re-read the array (fresh etag), scan it for a route whose host matcher OVERLAPS
// hostname OR that carries the captured route's @id, and if the name is already
// claimed REFUSE with ErrRestoreNameClaimed — never a blind append, because Caddy
// permits overlapping host matchers, so appending onto a live claim would create
// silent DUAL exposure the after-the-fact verify could only report, not prevent.
// Only when the name is genuinely free does it POST-append the EXACT captured
// bytes (never a re-serialization — byte-faithful even for fields tailport does
// not model) under that same array etag; a concurrent append moving the array
// hash → 412 → re-read, re-scan, retry (bounded). Refusing before appending under
// the array If-Match makes the refuse-vs-append decision atomic against the
// overlap race rather than append-then-discover.
//
// It is A-before-B by contract: an OWNED captured route shares our take-over's
// @id, so the caller MUST have removed the take-over (step A) first, else this
// scan finds the duplicate @id and refuses. If the guarding array ETag is empty
// (a pre-2.5.2 edge that ignores If-Match) it refuses with ErrEdgeNoIfMatch
// rather than append blind, mirroring PurgeConflict.
func (c *Client) RestoreRoute(ctx context.Context, hostname string, captured json.RawMessage) error {
	hostname = canonHost(hostname)
	capID := rawRouteID(captured)
	for attempt := 0; attempt < maxRetries; attempt++ {
		raws, arrEtag, err := c.fetchRoutesRaw(ctx)
		if err != nil {
			return err
		}
		// Scan for a re-claim: any live route overlapping the hostname, or one
		// carrying the captured route's @id (a duplicate-@id append would be
		// rejected by Caddy anyway, but refusing here keeps the message honest).
		for _, elem := range raws {
			var r Route
			if json.Unmarshal(elem, &r) != nil {
				continue // an element we can't decode can't be matched; skip it
			}
			if routeOverlaps(r, hostname) {
				return ErrRestoreNameClaimed
			}
			if capID != "" && rawRouteID(elem) == capID {
				return ErrRestoreNameClaimed
			}
		}
		// The name is genuinely free. Refuse to append with no guarding ETag (a
		// pre-2.5.2 edge ignores If-Match, so the append could race a concurrent
		// claim), mirroring PurgeConflict's ErrEdgeNoIfMatch guard.
		if arrEtag == "" {
			return ErrEdgeNoIfMatch
		}
		// POST-append the EXACT captured bytes (byte-faithful; never re-serialized)
		// under the array etag. A concurrent array change → 412 → re-read/re-scan.
		body, status, _, err := c.do(ctx, http.MethodPost, c.routesURL(), captured, arrEtag)
		if err != nil {
			return err
		}
		if status == http.StatusPreconditionFailed {
			continue // array moved under us; re-read, re-scan, retry
		}
		if status < 200 || status >= 300 {
			return c.statusError(status, body)
		}
		return nil
	}
	return ErrConcurrentUpdate
}

// RestoreState classifies the OBSERVED post-restore edge state (kata ttfh; design
// §3.6-C). The undo message is computed from this — a real content compare over a
// full-array re-read — never from a guess about which step failed: a confidently
// wrong "restored" is worse than an honest "couldn't".
type RestoreState int

const (
	// Restored means exactly one route overlaps the hostname AND it semantically
	// equals the captured bytes (a normalized-JSON compare INCLUDING fields
	// tailport doesn't model) AND no OTHER route overlaps it. Only this ⇒ the undo
	// may honestly say "restored" (content; never positional/routing fidelity — a
	// restored route re-appends at the array end, an accepted loss).
	Restored RestoreState = iota
	// RestoreUnclaimed means nothing overlaps the hostname: the restore did not
	// take (or the name was freed again after B).
	RestoreUnclaimed
	// RestoreClaimedByOther means a DIFFERENT or additional route now overlaps the
	// hostname (a third-party re-take, or a second overlapping route appended
	// between B and C creating dual exposure) — resolve the drift in Caddy.
	RestoreClaimedByOther
	// RestoreContentMismatch means a route carrying the captured route's @id is
	// present for the hostname but its content DIFFERS from the captured bytes — a
	// same-@id PATCH between B and C swapped the backend/auth/an unmodeled field.
	// "present" is not "restored".
	RestoreContentMismatch
)

// String renders a RestoreState for test output and diagnostics.
func (s RestoreState) String() string {
	switch s {
	case Restored:
		return "Restored"
	case RestoreUnclaimed:
		return "Unclaimed"
	case RestoreClaimedByOther:
		return "ClaimedByOther"
	case RestoreContentMismatch:
		return "ContentMismatch"
	default:
		return "RestoreState(" + strconv.Itoa(int(s)) + ")"
	}
}

// VerifyRestore is undo step C (kata ttfh; design §3.6-C): a full-array re-read
// that classifies the observed post-restore state, so the caller's message is
// computed from THIS, not from a guess about which step failed. It deliberately
// does NOT reuse InspectConflict — that short-circuits on finding our @id and
// would miss an ADDITIONAL overlapping route appended between B and C. It scans
// the WHOLE array for routes overlapping hostname and classifies:
//
//   - exactly one overlap that SEMANTICALLY EQUALS captured ⇒ Restored;
//   - none ⇒ Unclaimed;
//   - one overlap carrying captured's @id but content differs ⇒ ContentMismatch
//     (a same-@id PATCH swapped the backend/auth/an unmodeled field);
//   - anything else (a different route holds it, or >1 route overlaps) ⇒
//     ClaimedByOther.
//
// The content compare is a normalized-JSON equality (semanticallyEqual) that
// includes fields tailport doesn't model — not hostMatcherIs — so "present" is
// never mistaken for "restored".
func (c *Client) VerifyRestore(ctx context.Context, hostname string, captured json.RawMessage) (RestoreState, error) {
	hostname = canonHost(hostname)
	raws, _, err := c.fetchRoutesRaw(ctx)
	if err != nil {
		return 0, err
	}
	var overlapping []json.RawMessage
	for _, elem := range raws {
		var r Route
		if json.Unmarshal(elem, &r) != nil {
			continue // an element we can't decode can't overlap; skip it
		}
		if routeOverlaps(r, hostname) {
			overlapping = append(overlapping, elem)
		}
	}
	switch len(overlapping) {
	case 0:
		return RestoreUnclaimed, nil
	case 1:
		if semanticallyEqual(overlapping[0], captured) {
			return Restored, nil
		}
		// Present but not equal: our @id but mutated content ⇒ ContentMismatch;
		// otherwise a different route holds the name ⇒ ClaimedByOther.
		if capID := rawRouteID(captured); capID != "" && rawRouteID(overlapping[0]) == capID {
			return RestoreContentMismatch, nil
		}
		return RestoreClaimedByOther, nil
	default:
		// More than one route overlaps: a concurrent overlapping append between B
		// and C created dual exposure — even if one of them equals captured, the
		// name is not cleanly ours, so this is never "restored".
		return RestoreClaimedByOther, nil
	}
}

// semanticallyEqual reports whether two route elements are equal as JSON VALUES —
// independent of key ordering and whitespace, and INCLUDING every field (so a
// field tailport doesn't model still participates). It decodes both to generic
// values and re-marshals canonically (Go sorts object keys), then compares bytes;
// this is the honest content compare undo step C needs (design §3.6-C), not the
// structural hostMatcherIs. A decode/encode failure is treated as "not equal".
func semanticallyEqual(a, b json.RawMessage) bool {
	var av, bv interface{}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ab, err1 := json.Marshal(av)
	bb, err2 := json.Marshal(bv)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

// locateByExpect finds the index of the route the user approved to purge,
// identifying it the way it was CLASSIFIED rather than by first host-overlap
// (roborev hped #4). An @id-identified route is found by its @id; an id-less
// foreign route pinned by RawHash is found by that exact-bytes hash; a foreign
// route classified with neither identity falls back to the first host-overlap
// (the pre-hped behavior, correct when only one route can overlap). Returns -1
// when the approved route is not present.
func locateByExpect(raws []json.RawMessage, hostname string, expect PurgeExpect) int {
	switch {
	case expect.ID != "":
		for i, elem := range raws {
			if rawRouteID(elem) == expect.ID {
				return i
			}
		}
	case expect.RawHash != "":
		for i, elem := range raws {
			if rawIdentityHash(elem) == expect.RawHash {
				return i
			}
		}
	default:
		for i, elem := range raws {
			var r Route
			if json.Unmarshal(elem, &r) == nil && routeOverlaps(r, hostname) {
				return i
			}
		}
	}
	return -1
}

// rawsOverlap reports whether any raw route element overlaps the (already
// canonicalized) hostname — used to distinguish "the approved route is gone but
// another still claims the host" (→ ErrConflictChanged) from "nothing overlaps"
// (→ ErrNoConflict).
func rawsOverlap(raws []json.RawMessage, hostname string) bool {
	for _, elem := range raws {
		var r Route
		if json.Unmarshal(elem, &r) == nil && routeOverlaps(r, hostname) {
			return true
		}
	}
	return false
}

// fetchRoutesRaw GETs the shared routes array as raw elements (each preserving
// any field tailport doesn't model) and the array's path-scoped Etag for a
// subsequent index-DELETE If-Match. A 404 or empty body is an empty array. It is
// the raw-fidelity twin of fetchRoutes, used by PurgeConflict so capture is
// byte-faithful and per-element @id/host reads only what they need.
func (c *Client) fetchRoutesRaw(ctx context.Context) (routes []json.RawMessage, etag string, err error) {
	body, status, etag, err := c.do(ctx, http.MethodGet, c.routesURL(), nil, "")
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusNotFound {
		return nil, etag, nil
	}
	if status < 200 || status >= 300 {
		return nil, "", c.statusError(status, body)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, etag, nil
	}
	if err := json.Unmarshal(body, &routes); err != nil {
		return nil, "", fmt.Errorf("parsing caddy routes: %w", err)
	}
	return routes, etag, nil
}

// rawRouteID extracts a route element's @id, reading only that key ("" if absent
// or the element doesn't parse). Cheaper and more forgiving than a full Route
// decode when only ownership/identity is needed.
func rawRouteID(raw json.RawMessage) string {
	var m struct {
		ID string `json:"@id"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.ID
}

// rawIdentityHash is a stable, opaque fingerprint of a route element's exact
// bytes, used as an id-less foreign route's PurgeExpect identity (PurgeExpect.
// RawHash). It is not cryptographic — only a change detector — so a non-crypto
// hash keeps the zero-dep rule and is plenty.
func rawIdentityHash(raw json.RawMessage) string {
	h := fnv.New64a()
	_, _ = h.Write(raw)
	return strconv.FormatUint(h.Sum64(), 16)
}

// firstHandler returns the name of a route's first handler ("" if it has none),
// used to name a foreign route whose backend dial can't (a static_response,
// file_server, …).
func firstHandler(r Route) string {
	if len(r.Handle) == 0 {
		return ""
	}
	return r.Handle[0].Handler
}

// matcherHostList returns every host value across a route's match blocks, in
// order (nil if it carries no host matcher at all). It is the list form behind
// both matcherHosts (a comma-joined description) and ConflictInfo.Hosts (the
// blast-radius disclosure, roborev hped #3).
func matcherHostList(r Route) []string {
	var hosts []string
	for _, m := range r.Match {
		hosts = append(hosts, m.Host...)
	}
	return hosts
}

// matcherHosts joins every host value across a route's match blocks, comma-
// separated, for describing where a hijacked @id now points ("" if it carries no
// host matcher at all — e.g. a matcher of only path/method).
func matcherHosts(r Route) string {
	return strings.Join(matcherHostList(r), ", ")
}

// routeOverlaps reports whether any host matcher on r overlaps the already-
// canonicalized hostname, using the same predicate (hostsOverlap) findHostConflict
// applies — see hostsOverlap for the modeled matcher forms.
func routeOverlaps(r Route, hostname string) bool {
	for _, m := range r.Match {
		for _, h := range m.Host {
			if hostsOverlap(canonHost(h), hostname) {
				return true
			}
		}
	}
	return false
}

// parseRoute flattens a wire Route into a RouteInfo. ok is false when the route
// lacks a host matcher or a parseable reverse_proxy backend.
func parseRoute(r Route) (RouteInfo, bool) {
	if len(r.Match) == 0 || len(r.Match[0].Host) == 0 {
		return RouteInfo{}, false
	}
	label, port, ok := backendOf(r)
	if !ok {
		return RouteInfo{}, false
	}
	return RouteInfo{
		Hostname: canonHost(r.Match[0].Host[0]),
		Label:    label,
		Port:     port,
		Auth:     hasAuth(r),
		Owned:    strings.HasPrefix(r.ID, idPrefix),
	}, true
}

// hasAuth reports whether the route carries an authentication handler.
func hasAuth(r Route) bool {
	for _, h := range r.Handle {
		if h.Handler == "authentication" {
			return true
		}
	}
	return false
}

// backendOf extracts the backend label and port from a route's reverse_proxy
// dial. ok is false when there is no reverse_proxy handler or its dial doesn't
// parse as "<label>:<port>".
func backendOf(r Route) (label string, port int, ok bool) {
	for _, h := range r.Handle {
		if h.Handler == "reverse_proxy" && len(h.Upstreams) > 0 {
			return splitDial(h.Upstreams[0].Dial)
		}
	}
	return "", 0, false
}

// splitDial parses "<label>:<port>" into its parts.
func splitDial(dial string) (label string, port int, ok bool) {
	i := strings.LastIndex(dial, ":")
	if i <= 0 || i == len(dial)-1 {
		return "", 0, false
	}
	p, err := strconv.Atoi(dial[i+1:])
	if err != nil || p <= 0 {
		return "", 0, false
	}
	return dial[:i], p, true
}

// hostMatcherIs reports whether route's matcher is EXACTLY the single expected
// public hostname and nothing else: one match block whose only matcher key is
// host, carrying one host entry equal after canonicalization. A foreign edit
// that kept our @id but changed the host, added a second match block, or added
// any other matcher key (path, method, header, …) alongside host fails this
// check — so Publish/Unpublish refuse rather than mutate a route that is no
// longer purely our hostname. The extra-key check relies on Match.raw, which
// records every matcher key seen on decode.
func hostMatcherIs(route Route, hostname string) bool {
	if len(route.Match) != 1 {
		return false
	}
	m := route.Match[0]
	// The sole match block must contain exactly one key, and it must be host.
	// Any additional matcher (path, method, …) means this is more than our
	// hostname.
	if len(m.raw) != 1 {
		return false
	}
	if _, ok := m.raw["host"]; !ok {
		return false
	}
	return len(m.Host) == 1 && canonHost(m.Host[0]) == canonHost(hostname)
}

// findHostConflict returns a human description of the first route whose host
// matcher overlaps hostname, or "" if none does. Used only on the absent-@id
// path, where any match is by definition foreign or differently-owned.
//
// Overlap is judged the way Caddy's host matcher actually matches: hostnames
// are compared case-insensitively (via canonHost), the bare "*" matcher that
// matches every hostname is honored, and the common single-label leading
// wildcard "*.<suffix>" is honored — an existing "*.example.com" conflicts with
// a requested "app.example.com", and vice versa. hostname is expected already
// canonicalized by the caller.
//
// Boundary: this models exact (case-insensitive) matches, the bare "*"
// match-any matcher, and the single-label "*.<suffix>" wildcard (the form
// tailport can emit and the overwhelmingly common foreign form). It does NOT
// model MULTI-label wildcards, path/expression matchers, or routes with NO host
// matcher at all (catch-alls): Caddy resolves overlapping routes by route
// order, and treating "some catch-all exists" as a hard conflict for every
// publish would wrongly block legitimate publishes. Those broader cases are
// deliberately left for Caddy itself to resolve.
func findHostConflict(routes []Route, hostname string) string {
	for _, r := range routes {
		if routeOverlaps(r, hostname) {
			if r.ID != "" {
				return "route " + r.ID
			}
			return "a foreign route"
		}
	}
	return ""
}

// hostsOverlap reports whether two already-canonicalized Caddy host matchers
// can match the same request, covering case-insensitive exact equality, the
// bare "*" match-any matcher, and the single-label leading wildcard
// "*.<suffix>" in either position. See findHostConflict for the boundary on
// which matcher forms are modeled.
func hostsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	// A bare "*" host matcher matches every hostname, so it overlaps whatever
	// sits in the other position.
	if a == "*" || b == "*" {
		return true
	}
	if suffix, ok := wildcardSuffix(a); ok && oneLabelUnder(b, suffix) {
		return true
	}
	if suffix, ok := wildcardSuffix(b); ok && oneLabelUnder(a, suffix) {
		return true
	}
	return false
}

// wildcardSuffix reports whether h is a single-label leading wildcard matcher
// "*.<suffix>" (with a non-empty suffix) and returns that suffix.
func wildcardSuffix(h string) (suffix string, ok bool) {
	if rest, found := strings.CutPrefix(h, "*."); found && rest != "" {
		return rest, true
	}
	return "", false
}

// oneLabelUnder reports whether host is exactly "<one non-empty label>.suffix",
// i.e. host is one DNS label deeper than suffix — precisely the set a Caddy
// "*.suffix" wildcard matches (a single label, not "a.b.suffix").
func oneLabelUnder(host, suffix string) bool {
	rest, found := strings.CutSuffix(host, "."+suffix)
	if !found || rest == "" {
		return false
	}
	return !strings.Contains(rest, ".")
}
