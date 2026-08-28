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
			return ConflictInfo{Kind: None}, nil
		}
		info := ConflictInfo{Kind: OwnedDiffBackend, Owned: true, ID: route.ID, Handler: firstHandler(route)}
		if label, port, ok := backendOf(route); ok {
			info.Label, info.Port, info.BackendParseable = label, port, true
		}
		return info, nil
	}

	// Read 2: raw host-overlap scan. Match on the host matcher alone so a foreign
	// non-reverse_proxy route (which List/parseRoute would drop) is still found.
	routes, _, err := c.fetchRoutes(ctx)
	if err != nil {
		return ConflictInfo{}, err
	}
	for _, r := range routes {
		if !routeOverlaps(r, hostname) {
			continue
		}
		info := ConflictInfo{
			Kind:    ForeignOverlap,
			Owned:   strings.HasPrefix(r.ID, idPrefix),
			ID:      r.ID,
			Handler: firstHandler(r),
		}
		if label, port, ok := backendOf(r); ok {
			info.Label, info.Port, info.BackendParseable = label, port, true
		}
		return info, nil
	}

	// Nothing overlaps and our @id is clean: the conflict cleared.
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
// the structural fields ConflictInfo carries; it does not set RawHash (§3.1 keeps
// raw bytes out of the classifier).
func ExpectFromConflict(info ConflictInfo) PurgeExpect {
	return PurgeExpect{
		Owned:            info.Owned,
		ID:               info.ID,
		BackendParseable: info.BackendParseable,
		Label:            info.Label,
		Port:             info.Port,
		Handler:          info.Handler,
	}
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
// path-scoped etag, re-locates the host-overlapping route, and re-verifies it
// still matches expect — the identity classified at confirm time. If nothing
// overlaps → ErrNoConflict; if the live route drifted from expect (an
// owned→foreign escalation or an owned backend swap) → ErrConflictChanged. Only
// then does it capture the exact raw element bytes and delete: by /id/<id> under
// the id-scope etag when the route carries an @id (stable across index shifts),
// else by DELETE .../routes/<index> under the routes-array etag (the id-less
// foreign case; the parent-scope If-Match re-hashes the whole array, so any
// concurrent add/remove/reorder → 412 → re-read). A 412 or a vanished route
// re-reads and retries; the retry budget exhausting yields ErrConcurrentUpdate.
func (c *Client) PurgeConflict(ctx context.Context, hostname string, expect PurgeExpect) (Captured, error) {
	hostname = canonHost(hostname)
	for attempt := 0; attempt < maxRetries; attempt++ {
		raws, arrEtag, err := c.fetchRoutesRaw(ctx)
		if err != nil {
			return Captured{}, err
		}

		idx := -1
		var raw json.RawMessage
		var route Route
		for i, elem := range raws {
			var r Route
			if err := json.Unmarshal(elem, &r); err != nil {
				continue // an element we can't even decode can't be our overlap
			}
			if routeOverlaps(r, hostname) {
				idx, raw, route = i, elem, r
				break
			}
		}
		if idx < 0 {
			return Captured{}, ErrNoConflict
		}
		if !expect.matches(route, raw) {
			return Captured{}, ErrConflictChanged
		}

		// Capture the EXACT bytes before deleting (a copy — raw aliases the fetch
		// buffer). This is byte-faithful even if the route accreted fields tailport
		// doesn't model.
		captured := Captured{
			Raw:      append(json.RawMessage(nil), raw...),
			Hostname: hostname,
			HadID:    route.ID != "",
		}

		var body []byte
		var status int
		if route.ID != "" {
			// Prefer stable @id deletion. Fetch the id-scope etag; a 404 means the
			// route vanished between the array read and now → re-read and retry.
			live, idEtag, found, err := c.fetchByID(ctx, route.ID)
			if err != nil {
				return Captured{}, err
			}
			if !found {
				continue
			}
			// Re-verify against the id-read too, closing the array-read→id-read
			// window: if it drifted from expect here, don't delete it.
			if !expect.matches(live, raw) {
				return Captured{}, ErrConflictChanged
			}
			body, status, _, err = c.do(ctx, http.MethodDelete, c.idURL(route.ID), nil, idEtag)
			if err != nil {
				return Captured{}, err
			}
		} else {
			// Truly id-less foreign route: delete by array index under the
			// routes-array etag. The parent-scope If-Match re-hashes the whole
			// array, so an index-shifting concurrent edit → 412 → re-read.
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

// matcherHosts joins every host value across a route's match blocks, comma-
// separated, for describing where a hijacked @id now points ("" if it carries no
// host matcher at all — e.g. a matcher of only path/method).
func matcherHosts(r Route) string {
	var hosts []string
	for _, m := range r.Match {
		hosts = append(hosts, m.Host...)
	}
	return strings.Join(hosts, ", ")
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
