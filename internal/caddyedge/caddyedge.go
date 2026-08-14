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

// Match is a Caddy request matcher; only the host matcher is used here.
type Match struct {
	Host []string `json:"host"`
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

// hostMatcherIs reports whether route's matcher is exactly the single expected
// public hostname: one match block, one host entry, equal after
// canonicalization. A foreign edit that kept our @id but changed or added host
// matchers fails this check, so Publish/Unpublish can refuse rather than
// mutate an unrelated route they no longer actually own the shape of.
func hostMatcherIs(route Route, hostname string) bool {
	return len(route.Match) == 1 &&
		len(route.Match[0].Host) == 1 &&
		canonHost(route.Match[0].Host[0]) == canonHost(hostname)
}

// findHostConflict returns a human description of the first route whose host
// matcher overlaps hostname, or "" if none does. Used only on the absent-@id
// path, where any match is by definition foreign or differently-owned.
//
// Overlap is judged the way Caddy's host matcher actually matches: hostnames
// are compared case-insensitively (via canonHost), and the common single-label
// leading wildcard "*.<suffix>" is honored — an existing "*.example.com"
// conflicts with a requested "app.example.com", and vice versa. hostname is
// expected already canonicalized by the caller.
//
// Boundary: this deliberately models only exact (case-insensitive) matches and
// the single-label "*.<suffix>" wildcard, which is the form tailport can emit
// and the overwhelmingly common foreign form. It does NOT model Caddy's full
// matcher grammar — a bare "*", multi-label wildcards, or path/expression
// matchers can still overlap without being reported here; those broader cases
// are left for Caddy itself to resolve.
func findHostConflict(routes []Route, hostname string) string {
	for _, r := range routes {
		for _, m := range r.Match {
			for _, h := range m.Host {
				if hostsOverlap(canonHost(h), hostname) {
					if r.ID != "" {
						return "route " + r.ID
					}
					return "a foreign route"
				}
			}
		}
	}
	return ""
}

// hostsOverlap reports whether two already-canonicalized Caddy host matchers
// can match the same request, covering case-insensitive exact equality and the
// single-label leading wildcard "*.<suffix>" in either position. See
// findHostConflict for the boundary on which matcher forms are modeled.
func hostsOverlap(a, b string) bool {
	if a == b {
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
