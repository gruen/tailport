package caddyedge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// --- pure-builder tests -----------------------------------------------------

func TestIDFor(t *testing.T) {
	if got := IDFor("myapp.example.com"); got != "tailport-myapp.example.com" {
		t.Errorf("IDFor = %q, want tailport-myapp.example.com", got)
	}
}

func TestValidHostname(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"myapp.example.com", true},
		{"a.b.example.com", true}, // nested subdomains allowed
		{"deep.a.b.example.com", true},
		{"host", true},                // single label is a valid hostname
		{"x-y.example.com", true},     // internal hyphen ok
		{"", false},                   // empty
		{"has space.com", false},      // whitespace
		{"http://x.com", false},       // scheme
		{"x.com/path", false},         // path
		{"x.example.com:8080", false}, // port
		{".example.com", false},       // leading dot
		{"example.com.", false},       // trailing dot
		{"a..b.com", false},           // doubled dot -> empty label
		{"-bad.com", false},           // label starts with hyphen
		{"bad-.com", false},           // label ends with hyphen
		{"under_score.com", false},    // underscore not LDH
	}
	for _, c := range cases {
		if got := ValidHostname(c.in); got != c.ok {
			t.Errorf("ValidHostname(%q) = %v, want %v", c.in, got, c.ok)
		}
	}
}

// TestValidLabel pins the exported single-label validator (kata ztzg) directly
// -- internal/ui's entryPublishHostname step calls this, not ValidHostname, to
// validate a typed caddy.hostname: it must accept a hyphenated MagicDNS label
// like "caddy-on-fly" and reject blank or dotted/FQDN-shaped input outright
// (an FQDN silently 403s the edge's admin API, which only admits its short
// name).
func TestValidLabel(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"caddy", true},
		{"caddy-on-fly", true},           // hyphenated label -- the motivating example
		{"x-y", true},                    // internal hyphen ok
		{"", false},                      // blank
		{"caddy.tailnet.ts.net", false},  // FQDN-shaped -- contains dots
		{"-bad", false},                  // leading hyphen
		{"bad-", false},                  // trailing hyphen
		{"under_score", false},           // underscore not LDH
		{strings.Repeat("a", 64), false}, // over the 63-char label max
	}
	for _, c := range cases {
		if got := ValidLabel(c.in); got != c.ok {
			t.Errorf("ValidLabel(%q) = %v, want %v", c.in, got, c.ok)
		}
	}
}

// TestBuildRouteSchemeAlwaysHTTP is the load-bearing invariant test: the emitted
// upstream is a plain dial with NO tls/transport, and there is no API to request
// https. It also pins the Host rewrite and @id.
func TestBuildRouteSchemeAlwaysHTTP(t *testing.T) {
	r := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)

	// Plain http: a bare dial, and absolutely no TLS/transport anywhere.
	if !strings.Contains(js, `"dial":"dev-box:3000"`) {
		t.Errorf("route JSON missing plain dial: %s", js)
	}
	if strings.Contains(js, "transport") || strings.Contains(js, "tls") {
		t.Errorf("route JSON must have no tls/transport (scheme is always http): %s", js)
	}
	// @id serializes as the literal "@id" key.
	if !strings.Contains(js, `"@id":"tailport-myapp.example.com"`) {
		t.Errorf("route JSON missing literal @id key: %s", js)
	}
	if r.ID != "tailport-myapp.example.com" {
		t.Errorf("ID = %q, want tailport-myapp.example.com", r.ID)
	}
}

func TestBuildRouteHostHeaderIsBackendNotPublic(t *testing.T) {
	r := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	// Exactly one handler (no auth): the reverse_proxy.
	if len(r.Handle) != 1 || r.Handle[0].Handler != "reverse_proxy" {
		t.Fatalf("handle chain = %+v, want a single reverse_proxy", r.Handle)
	}
	set := r.Handle[0].Headers.Request.Set["Host"]
	if len(set) != 1 || set[0] != "dev-box:3000" {
		t.Errorf("upstream Host = %v, want [dev-box:3000] (backend, not public hostname)", set)
	}
	if set[0] == "myapp.example.com" {
		t.Errorf("upstream Host must not be the public hostname")
	}
	if !r.Terminal {
		t.Errorf("route must be terminal")
	}
}

func TestBuildRouteAuthOrdering(t *testing.T) {
	// No auth: single reverse_proxy handler.
	if got := BuildRoute("h.example.com", "box", 80, nil); hasAuth(got) {
		t.Errorf("nil auth must not emit an authentication handler")
	}

	// With auth: the authentication handler is present and ordered BEFORE
	// reverse_proxy, carrying the pre-hashed bcrypt password (never plaintext).
	r := BuildRoute("h.example.com", "box", 80, &BasicAuth{User: "alice", Hash: "$2a$10$abcdefghijklmnopqrstuv"})
	if len(r.Handle) != 2 {
		t.Fatalf("handle chain len = %d, want 2 (auth then reverse_proxy)", len(r.Handle))
	}
	if r.Handle[0].Handler != "authentication" || r.Handle[1].Handler != "reverse_proxy" {
		t.Fatalf("handler order = [%s %s], want [authentication reverse_proxy]", r.Handle[0].Handler, r.Handle[1].Handler)
	}
	acct := r.Handle[0].Providers.HTTPBasic.Accounts[0]
	if acct.Username != "alice" || acct.Password != "$2a$10$abcdefghijklmnopqrstuv" {
		t.Errorf("account = %+v, want alice with the bcrypt hash", acct)
	}
	if r.Handle[0].Providers.HTTPBasic.Hash.Algorithm != "bcrypt" {
		t.Errorf("hash algorithm = %q, want bcrypt", r.Handle[0].Providers.HTTPBasic.Hash.Algorithm)
	}
}

// --- fake admin API ---------------------------------------------------------

// fakeAdmin is an in-process model of the Caddy admin API surface caddyedge
// uses, rebuilt for kata 6n15 (design §7 / review r1-F7) to faithfully mirror
// the mechanics purge/capture/If-Match rest on:
//
//   - Routes are stored as raw json.RawMessage ELEMENTS, so a route carrying a
//     field tailport doesn't model round-trips byte-for-byte (capture fidelity).
//   - ETags are PATH-SCOPED the way real Caddy emits them (PR #4579): a GET or a
//     mutation returns Etag "<path> <hash>"; a mutating request's If-Match carries
//     "<path> <hash>", and precheck re-hashes the config AT THE PATH EMBEDDED IN
//     THE If-Match VALUE (not the request URL) — which is exactly what lets a
//     parent routes-array If-Match guard a child index DELETE.
//   - POST rejects a duplicate @id with a Caddy-style 4xx (an @id is unique).
//   - It supports index-DELETE (.../routes/<i>, delete-and-shift, bounds-checked)
//     alongside /id DELETE, so the id-less-foreign purge path is real.
//
// It still records whether any client mutation actually occurred, so "no
// mutation" assertions remain real.
type fakeAdmin struct {
	mu     sync.Mutex
	server string            // server name embedded in the routes path
	routes []json.RawMessage // the shared routes array, raw elements

	// force412, when > 0, injects a precondition failure on each of the next
	// N client mutations (simulating a concurrent edit), letting tests exercise
	// the 412 re-read/retry loop deterministically.
	force412 int
	// deleteReturns404, when true, makes DELETE answer 404 without removing the
	// route, to exercise the "404 on DELETE = success" branch.
	deleteReturns404 bool
	// noEtag, when true, suppresses the Etag header on GET responses, modeling a
	// pre-2.5.2 edge that never emits an ETag — so a client's guarding etag is
	// empty and a force-delete must REFUSE (roborev hped #6).
	noEtag bool
	// hookAfterRoutesGet, when set, fires ONCE right after a GET on the routes-
	// array path is served, letting a test simulate a concurrent edit that lands
	// in the array-read→/id-read window (roborev hped #1). It runs while f.mu is
	// held, so it must mutate f.routes directly and MUST NOT lock f.mu.
	hookAfterRoutesGet func()

	// Recorded, accepted client mutations (a rejected 412 is NOT counted).
	mutations int
	post      int
	patch     int
	del       int
	put       int      // any PUT attempt at all — the code must never issue one
	delPaths  []string // request paths of accepted DELETEs, to assert /id vs index
}

func (f *fakeAdmin) routesPath() string {
	return "/config/apps/http/servers/" + f.server + "/routes"
}

func (f *fakeAdmin) indexOf(id string) int {
	for i, raw := range f.routes {
		if rawRouteID(raw) == id {
			return i
		}
	}
	return -1
}

// routeAt decodes the i-th stored element into a typed Route, for the few
// existing assertions that reach past len() into a route's fields.
func (f *fakeAdmin) routeAt(i int) Route {
	var r Route
	_ = json.Unmarshal(f.routes[i], &r)
	return r
}

// hashAt returns a stable hash of the config at path — the whole routes array,
// an /id element, or an index element — mirroring how real Caddy scopes an ETag
// to a config path. exists is false when the path names nothing.
func (f *fakeAdmin) hashAt(path string) (hash string, exists bool) {
	switch {
	case path == f.routesPath():
		b, _ := json.Marshal(f.routes)
		return rawIdentityHash(b), true
	case strings.HasPrefix(path, "/id/"):
		idx := f.indexOf(strings.TrimPrefix(path, "/id/"))
		if idx < 0 {
			return "", false
		}
		return rawIdentityHash(f.routes[idx]), true
	case strings.HasPrefix(path, f.routesPath()+"/"):
		i, err := strconv.Atoi(strings.TrimPrefix(path, f.routesPath()+"/"))
		if err != nil || i < 0 || i >= len(f.routes) {
			return "", false
		}
		return rawIdentityHash(f.routes[i]), true
	}
	return "", false
}

// etagFor renders the path-scoped ETag real Caddy emits: "<path> <hash>".
func (f *fakeAdmin) etagFor(path string) string {
	h, _ := f.hashAt(path)
	return `"` + path + " " + h + `"`
}

// splitIfMatch splits a `"<path> <hash>"` If-Match value into its path and hash.
// Both halves are space-free (a URL path and a hex hash), so a single Cut on the
// first space is unambiguous.
func splitIfMatch(im string) (path, hash string, ok bool) {
	im = strings.Trim(im, `"`)
	return strings.Cut(im, " ")
}

// precheck enforces PATH-SCOPED optimistic concurrency for a mutation. It parses
// the client's If-Match ("<path> <hash>") and re-hashes the config at the
// EMBEDDED path (not the request URL — this is what makes a parent routes-array
// If-Match correctly guard a child index DELETE), writing a 412 on mismatch. A
// pending force412 injects a precondition failure to exercise the retry loop. An
// empty If-Match is allowed (an unconditional write / a pre-2.5.2 edge).
func (f *fakeAdmin) precheck(w http.ResponseWriter, r *http.Request) bool {
	if f.force412 > 0 {
		f.force412--
		w.Header().Set("Etag", f.etagFor(f.routesPath()))
		http.Error(w, "config changed under you", http.StatusPreconditionFailed)
		return false
	}
	im := r.Header.Get("If-Match")
	if im == "" {
		return true
	}
	path, hash, ok := splitIfMatch(im)
	cur, exists := f.hashAt(path)
	if !ok || !exists || cur != hash {
		w.Header().Set("Etag", f.etagFor(f.routesPath()))
		http.Error(w, "etag mismatch", http.StatusPreconditionFailed)
		return false
	}
	return true
}

func (f *fakeAdmin) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/id/"):
		f.handleID(w, r, strings.TrimPrefix(r.URL.Path, "/id/"))
	case r.URL.Path == f.routesPath():
		f.handleRoutes(w, r)
	case strings.HasPrefix(r.URL.Path, f.routesPath()+"/"):
		f.handleRouteIndex(w, r, strings.TrimPrefix(r.URL.Path, f.routesPath()+"/"))
	default:
		http.Error(w, "unknown path", http.StatusNotFound)
	}
}

func (f *fakeAdmin) handleID(w http.ResponseWriter, r *http.Request, id string) {
	idx := f.indexOf(id)
	switch r.Method {
	case http.MethodGet:
		if idx < 0 {
			http.Error(w, "unknown object id", http.StatusNotFound)
			return
		}
		if !f.noEtag {
			w.Header().Set("Etag", f.etagFor("/id/"+id))
		}
		f.writeJSON(w, f.routes[idx])
	case http.MethodPatch:
		if !f.precheck(w, r) {
			return
		}
		if idx < 0 {
			http.Error(w, "unknown object id", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.routes[idx] = json.RawMessage(body) // store raw bytes verbatim
		f.mutations++
		f.patch++
		w.Header().Set("Etag", f.etagFor("/id/"+id))
	case http.MethodDelete:
		if !f.precheck(w, r) {
			return
		}
		if f.deleteReturns404 {
			http.Error(w, "unknown object id", http.StatusNotFound)
			return
		}
		if idx < 0 {
			http.Error(w, "unknown object id", http.StatusNotFound)
			return
		}
		f.routes = append(f.routes[:idx], f.routes[idx+1:]...)
		f.mutations++
		f.del++
		f.delPaths = append(f.delPaths, "/id/"+id)
		w.Header().Set("Etag", f.etagFor(f.routesPath()))
	case http.MethodPut:
		// PUT is strict-create in Caddy and would clobber a sibling's edit;
		// caddyedge must never issue it. Record and reject.
		f.put++
		http.Error(w, "PUT is unsupported by this fake", http.StatusBadRequest)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeAdmin) handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !f.noEtag {
			w.Header().Set("Etag", f.etagFor(f.routesPath()))
		}
		f.writeJSON(w, f.routes)
		if f.hookAfterRoutesGet != nil {
			h := f.hookAfterRoutesGet
			f.hookAfterRoutesGet = nil // fire once
			h()
		}
	case http.MethodPost:
		if !f.precheck(w, r) {
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// An @id is unique across the whole config; a duplicate POST is rejected
		// by real Caddy with a 4xx, not appended.
		if id := rawRouteID(body); id != "" && f.indexOf(id) >= 0 {
			http.Error(w, "loading config: duplicate id: "+id, http.StatusBadRequest)
			return
		}
		f.routes = append(f.routes, json.RawMessage(body)) // append raw bytes verbatim
		f.mutations++
		f.post++
		w.Header().Set("Etag", f.etagFor(f.routesPath()))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleRouteIndex serves DELETE /config/.../routes/<index>: delete-and-shift,
// bounds-checked, under the parent routes-array If-Match (precheck).
func (f *fakeAdmin) handleRouteIndex(w http.ResponseWriter, r *http.Request, idxStr string) {
	switch r.Method {
	case http.MethodDelete:
		if !f.precheck(w, r) {
			return
		}
		i, err := strconv.Atoi(idxStr)
		if err != nil || i < 0 || i >= len(f.routes) {
			http.Error(w, "invalid route index", http.StatusBadRequest)
			return
		}
		f.routes = append(f.routes[:i], f.routes[i+1:]...) // delete-and-shift
		f.mutations++
		f.del++
		f.delPaths = append(f.delPaths, f.routesPath()+"/"+idxStr)
		w.Header().Set("Etag", f.etagFor(f.routesPath()))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// newFake starts a fakeAdmin httptest.Server seeded with typed routes (marshaled
// to raw at seed time) and returns a Client wired to it plus the fake (for
// mutation assertions).
func newFake(t *testing.T, routes ...Route) (*Client, *fakeAdmin) {
	t.Helper()
	raws := make([]json.RawMessage, 0, len(routes))
	for _, r := range routes {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("seed marshal: %v", err)
		}
		raws = append(raws, b)
	}
	return newFakeRaw(t, raws...)
}

// newFakeRaw is the raw-seeding path: it seeds the fake with exact element bytes,
// so a test can hold a route carrying a field tailport doesn't model and assert
// it round-trips through capture unchanged.
func newFakeRaw(t *testing.T, raws ...json.RawMessage) (*Client, *fakeAdmin) {
	t.Helper()
	f := &fakeAdmin{server: "tailport", routes: append([]json.RawMessage(nil), raws...)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{HTTPClient: srv.Client(), AdminURL: srv.URL, ServerName: "tailport"}, f
}

// --- Client tests -----------------------------------------------------------

func TestPublishCreatesWhenAbsent(t *testing.T) {
	c, f := newFake(t)
	if err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if f.post != 1 || f.mutations != 1 {
		t.Errorf("expected exactly one POST, got post=%d mutations=%d", f.post, f.mutations)
	}
	if len(f.routes) != 1 || f.routeAt(0).ID != "tailport-myapp.example.com" {
		t.Errorf("route not appended correctly: %+v", f.routes)
	}
}

func TestPublishIdempotentSameBackendPatches(t *testing.T) {
	// A tailport route for the same host + backend already exists.
	existing := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	c, f := newFake(t, existing)

	if err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Idempotent republish MUST be a PATCH — never PUT, never a second POST.
	if f.patch != 1 {
		t.Errorf("expected exactly one PATCH, got patch=%d", f.patch)
	}
	if f.post != 0 || f.put != 0 {
		t.Errorf("idempotent republish must not POST or PUT: post=%d put=%d", f.post, f.put)
	}
	if len(f.routes) != 1 {
		t.Errorf("route count changed: %+v", f.routes)
	}
}

func TestPublishDifferentBackendHostConflictsNoMutation(t *testing.T) {
	// Same public hostname, but tailport already points it at a different box.
	existing := BuildRoute("myapp.example.com", "other-box", 3000, nil)
	c, f := newFake(t, existing)

	err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if !strings.Contains(err.Error(), "other-box:3000") {
		t.Errorf("conflict error should name the current backend, got %q", err.Error())
	}
	if f.mutations != 0 {
		t.Errorf("conflict must not mutate, got %d mutations", f.mutations)
	}
}

func TestPublishDifferentPortConflictsNoMutation(t *testing.T) {
	existing := BuildRoute("myapp.example.com", "dev-box", 9999, nil)
	c, f := newFake(t, existing)

	err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if !strings.Contains(err.Error(), "dev-box:9999") {
		t.Errorf("conflict error should name the current port, got %q", err.Error())
	}
	if f.mutations != 0 {
		t.Errorf("conflict must not mutate, got %d mutations", f.mutations)
	}
}

func TestPublishForeignRouteConflictsNoMutation(t *testing.T) {
	// A non-tailport route already claims the hostname (no tailport- @id).
	foreign := Route{
		ID:    "someone-elses-route",
		Match: []Match{{Host: []string{"myapp.example.com"}}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}},
		}},
		Terminal: true,
	}
	c, f := newFake(t, foreign)

	err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if f.mutations != 0 {
		t.Errorf("foreign conflict must not mutate, got %d mutations", f.mutations)
	}
}

func TestPublishSimultaneousCreate412Retries(t *testing.T) {
	c, f := newFake(t)
	f.force412 = 1 // first POST loses the race, then the retry wins

	if err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil); err != nil {
		t.Fatalf("Publish should have retried and succeeded: %v", err)
	}
	if f.force412 != 0 {
		t.Errorf("the injected 412 was not consumed (force412=%d)", f.force412)
	}
	if f.post != 1 || f.mutations != 1 {
		t.Errorf("expected exactly one accepted POST after the retry, got post=%d mutations=%d", f.post, f.mutations)
	}
	if len(f.routes) != 1 {
		t.Errorf("expected exactly one route after retry, got %d", len(f.routes))
	}
}

func TestPublish412RetriesExhausted(t *testing.T) {
	c, f := newFake(t)
	f.force412 = maxRetries + 2 // every attempt loses the race

	err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("err = %v, want ErrConcurrentUpdate", err)
	}
	if f.mutations != 0 {
		t.Errorf("no mutation should have been accepted, got %d", f.mutations)
	}
	if len(f.routes) != 0 {
		t.Errorf("no route should have been created, got %d", len(f.routes))
	}
}

func TestUnpublishDeletesWhenOwnedAndMatching(t *testing.T) {
	existing := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	c, f := newFake(t, existing)

	if err := c.Unpublish(context.Background(), "myapp.example.com", "dev-box", 3000); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if f.del != 1 || f.mutations != 1 {
		t.Errorf("expected exactly one DELETE, got del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.routes) != 0 {
		t.Errorf("route should be gone, got %+v", f.routes)
	}
}

func TestUnpublishAbsentIsNotFound(t *testing.T) {
	c, f := newFake(t)
	err := c.Unpublish(context.Background(), "gone.example.com", "dev-box", 3000)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if f.mutations != 0 {
		t.Errorf("absent unpublish must not mutate, got %d", f.mutations)
	}
}

func TestUnpublishStaleStateRefusesNoDelete(t *testing.T) {
	// The route moved to a different backend since the UI last saw it.
	existing := BuildRoute("myapp.example.com", "new-box", 3000, nil)
	c, f := newFake(t, existing)

	err := c.Unpublish(context.Background(), "myapp.example.com", "dev-box", 3000)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict (refuse stale delete)", err)
	}
	if f.del != 0 || f.mutations != 0 {
		t.Errorf("stale unpublish must NOT delete, got del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.routes) != 1 {
		t.Errorf("route must survive a refused unpublish, got %d", len(f.routes))
	}
}

func TestUnpublish404OnDeleteIsSuccess(t *testing.T) {
	existing := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	c, f := newFake(t, existing)
	f.deleteReturns404 = true // someone deleted it between our GET and DELETE

	if err := c.Unpublish(context.Background(), "myapp.example.com", "dev-box", 3000); err != nil {
		t.Fatalf("404 on DELETE should be success, got %v", err)
	}
}

func TestListParsesAndSkips(t *testing.T) {
	owned := BuildRoute("owned.example.com", "dev-box", 3000, &BasicAuth{User: "u", Hash: "$2a$x"})
	foreign := Route{
		ID:     "foreign",
		Match:  []Match{{Host: []string{"foreign.example.com"}}},
		Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.1:80"}}}},
	}
	// Unparseable: no reverse_proxy backend -> must be skipped, not fatal.
	junk := Route{ID: "junk", Match: []Match{{Host: []string{"junk.example.com"}}}}

	c, _ := newFake(t, owned, foreign, junk)
	infos, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List returned %d routes, want 2 (junk skipped): %+v", len(infos), infos)
	}

	byHost := map[string]RouteInfo{}
	for _, i := range infos {
		byHost[i.Hostname] = i
	}
	o := byHost["owned.example.com"]
	if o.Label != "dev-box" || o.Port != 3000 || !o.Auth || !o.Owned {
		t.Errorf("owned route parsed wrong: %+v", o)
	}
	fr := byHost["foreign.example.com"]
	if fr.Label != "10.0.0.1" || fr.Port != 80 || fr.Auth || fr.Owned {
		t.Errorf("foreign route parsed wrong: %+v", fr)
	}
}

func TestUnreachableAdmin(t *testing.T) {
	// Start a fake, capture its URL, then close it so the port refuses.
	c, _ := newFake(t)
	// Close the underlying server by pointing at a definitely-closed one.
	srv := httptest.NewServer(&fakeAdmin{server: "tailport"})
	closedURL := srv.URL
	srv.Close()
	c.AdminURL = closedURL

	err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

// TestPublishStaleHostMatcherConflictsNoMutation covers finding #1 for Publish:
// a foreign edit kept our deterministic @id but repointed the host matcher at a
// different public hostname (same backend, so only the matcher check can fire).
// Trusting the @id would overwrite an unrelated route; Publish must refuse.
func TestPublishStaleHostMatcherConflictsNoMutation(t *testing.T) {
	stale := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	stale.Match = []Match{{Host: []string{"evil.example.com"}}} // @id kept, matcher changed
	c, f := newFake(t, stale)

	err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if f.mutations != 0 {
		t.Errorf("stale host matcher must not mutate, got %d mutations", f.mutations)
	}
	if len(f.routes) != 1 || f.routeAt(0).Match[0].Host[0] != "evil.example.com" {
		t.Errorf("foreign-edited route must survive untouched, got %+v", f.routes)
	}
}

// TestUnpublishStaleHostMatcherRefusesNoDelete covers finding #1 for Unpublish:
// a foreign edit kept our @id but ADDED a second host matcher (backend still
// ours). Deleting on @id alone would remove an unrelated public route, so
// Unpublish must refuse with ErrHostnameConflict and delete nothing.
func TestUnpublishStaleHostMatcherRefusesNoDelete(t *testing.T) {
	stale := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	stale.Match[0].Host = append(stale.Match[0].Host, "extra.example.com") // @id kept, matcher widened
	c, f := newFake(t, stale)

	err := c.Unpublish(context.Background(), "myapp.example.com", "dev-box", 3000)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict (refuse stale-matcher delete)", err)
	}
	if f.del != 0 || f.mutations != 0 {
		t.Errorf("stale host matcher must NOT delete, got del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.routes) != 1 {
		t.Errorf("route must survive a refused unpublish, got %d", len(f.routes))
	}
}

// TestPublishCaseVariantForeignConflictsNoMutation covers finding #2 (casing):
// an existing foreign route claims the hostname in a different case. DNS host
// matching is case-insensitive, so this must be a conflict, not a silent
// second route that shadows or is shadowed by it.
func TestPublishCaseVariantForeignConflictsNoMutation(t *testing.T) {
	foreign := Route{
		ID:    "someone-elses-route",
		Match: []Match{{Host: []string{"App.Example.COM"}}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}},
		}},
		Terminal: true,
	}
	c, f := newFake(t, foreign)

	err := c.Publish(context.Background(), "app.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if f.mutations != 0 {
		t.Errorf("case-variant conflict must not mutate, got %d mutations", f.mutations)
	}
}

// TestPublishWildcardForeignConflictsNoMutation covers finding #2 (wildcard):
// an existing foreign "*.example.com" already covers the requested
// "app.example.com" under Caddy's single-label wildcard semantics.
func TestPublishWildcardForeignConflictsNoMutation(t *testing.T) {
	foreign := Route{
		ID:    "wildcard-route",
		Match: []Match{{Host: []string{"*.example.com"}}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}},
		}},
		Terminal: true,
	}
	c, f := newFake(t, foreign)

	err := c.Publish(context.Background(), "app.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if f.mutations != 0 {
		t.Errorf("wildcard conflict must not mutate, got %d mutations", f.mutations)
	}
}

// TestExtraMatcherKeyRefusesMutation covers finding #1: a live route under OUR
// deterministic @id and OUR backend, but whose sole match block carries an
// extra matcher key (path) alongside host. Decoding used to drop the path, so
// the route passed the ownership check — Publish would then broaden it (PATCH
// dropping the path constraint) and Unpublish would delete it, mutating a route
// that is NOT purely our hostname. Both must now refuse with zero mutation.
func TestExtraMatcherKeyRefusesMutation(t *testing.T) {
	// A route tailport built (our @id, dev-box:3000), then foreign-edited to add
	// a path matcher to its sole match block. json.Unmarshal populates Match.raw
	// with both keys, and Match.MarshalJSON preserves them across the fake's GET.
	seed := func() Route {
		r := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
		var m Match
		if err := json.Unmarshal([]byte(`{"host":["myapp.example.com"],"path":["/admin"]}`), &m); err != nil {
			t.Fatalf("seed unmarshal: %v", err)
		}
		r.Match = []Match{m}
		return r
	}

	t.Run("publish refuses", func(t *testing.T) {
		c, f := newFake(t, seed())
		err := c.Publish(context.Background(), "myapp.example.com", "dev-box", 3000, nil)
		if !errors.Is(err, ErrHostnameConflict) {
			t.Fatalf("err = %v, want ErrHostnameConflict", err)
		}
		if f.mutations != 0 {
			t.Errorf("extra matcher key must not mutate, got %d mutations", f.mutations)
		}
	})

	t.Run("unpublish refuses", func(t *testing.T) {
		c, f := newFake(t, seed())
		err := c.Unpublish(context.Background(), "myapp.example.com", "dev-box", 3000)
		if !errors.Is(err, ErrHostnameConflict) {
			t.Fatalf("err = %v, want ErrHostnameConflict", err)
		}
		if f.del != 0 || f.mutations != 0 {
			t.Errorf("extra matcher key must not delete, got del=%d mutations=%d", f.del, f.mutations)
		}
		if len(f.routes) != 1 {
			t.Errorf("route must survive a refused unpublish, got %d", len(f.routes))
		}
	})
}

// TestPublishBareWildcardForeignConflictsNoMutation covers finding #3: a foreign
// route matches EVERY hostname via a bare "*" host matcher. It overlaps the
// requested hostname, so Publish must refuse without mutating.
func TestPublishBareWildcardForeignConflictsNoMutation(t *testing.T) {
	foreign := Route{
		ID:    "catch-all",
		Match: []Match{{Host: []string{"*"}}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}},
		}},
		Terminal: true,
	}
	c, f := newFake(t, foreign)

	err := c.Publish(context.Background(), "app.example.com", "dev-box", 3000, nil)
	if !errors.Is(err, ErrHostnameConflict) {
		t.Fatalf("err = %v, want ErrHostnameConflict", err)
	}
	if f.mutations != 0 {
		t.Errorf("bare-* conflict must not mutate, got %d mutations", f.mutations)
	}
}

// TestHostsOverlap locks the overlap predicate directly, including the
// requested-wildcard direction and the single-label boundary (a "*.suffix"
// wildcard matches exactly one deeper label, not two).
func TestHostsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"app.example.com", "app.example.com", true},    // exact
		{"*", "app.example.com", true},                  // bare "*" matches any host
		{"app.example.com", "*", true},                  // bare "*" in either position
		{"*.example.com", "app.example.com", true},      // existing wildcard covers requested
		{"app.example.com", "*.example.com", true},      // requested wildcard covers existing
		{"*.example.com", "a.b.example.com", false},     // single-label wildcard: not two labels deep
		{"app.example.com", "other.example.com", false}, // sibling labels don't overlap
		{"*.example.com", "*.other.com", false},         // different wildcard suffixes
		{"*.example.com", "example.com", false},         // wildcard needs a leading label
	}
	for _, tc := range cases {
		if got := hostsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("hostsOverlap(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestUnreachableOnMidBodyRead covers finding #3: the server sends headers
// (Do() returns cleanly) then drops the connection mid-body, so the
// io.ReadAll of the response fails. That post-header transport failure must
// still map to ErrUnreachable, honoring the sentinel contract.
func TestUnreachableOnMidBodyRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("test server ResponseWriter is not a Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Promise 4096 bytes, deliver a fragment, then close early: the client
		// reads headers fine but the body read hits an unexpected EOF.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n")
		_, _ = buf.WriteString(`[{"partial":`)
		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	c := &Client{HTTPClient: srv.Client(), AdminURL: srv.URL, ServerName: "tailport"}
	_, err := c.List(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable (body read failed after headers)", err)
	}
}

// --- InspectConflict (kata qfbf) --------------------------------------------

// TestInspectConflictOwnedDiffBackend: our deterministic @id already holds the
// hostname but points at a DIFFERENT backend. Read 1 (fetchByID) finds our @id
// with a matcher that IS still ours, so the only reason Publish refused is the
// backend — Kind=OwnedDiffBackend, naming the current backend.
func TestInspectConflictOwnedDiffBackend(t *testing.T) {
	// Reuse the TestPublishDifferentBackend fixture: our @id -> other-box:3000.
	existing := BuildRoute("myapp.example.com", "other-box", 3000, nil)
	c, f := newFake(t, existing)

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != OwnedDiffBackend {
		t.Errorf("Kind = %v, want OwnedDiffBackend", info.Kind)
	}
	if !info.Owned || info.ID != "tailport-myapp.example.com" {
		t.Errorf("owned/ID wrong: owned=%v id=%q", info.Owned, info.ID)
	}
	if !info.BackendParseable || info.Label != "other-box" || info.Port != 3000 {
		t.Errorf("backend wrong: parseable=%v %s:%d, want other-box:3000", info.BackendParseable, info.Label, info.Port)
	}
	if info.Handler != "reverse_proxy" {
		t.Errorf("Handler = %q, want reverse_proxy", info.Handler)
	}
	if f.mutations != 0 {
		t.Errorf("InspectConflict is read-only, got %d mutations", f.mutations)
	}
}

// TestInspectConflictBackendAlreadyMatchesWanted (kata vsx4 #2): our @id already
// holds the hostname, its matcher is still exactly ours, and its backend is
// EXACTLY the one the caller is trying to publish (wantLabel:wantPort). Between
// the failed Publish and this classification read, the live route changed to
// point at the requested backend -- there is no conflict left to refuse, so
// Kind must be None (letting the caller's bounded retry PATCH it, applying its
// config) rather than a stale OwnedDiffBackend refusal. A route pointing
// elsewhere must still classify OwnedDiffBackend, unchanged.
func TestInspectConflictBackendAlreadyMatchesWanted(t *testing.T) {
	t.Run("backend now matches -> None", func(t *testing.T) {
		existing := BuildRoute("myapp.example.com", "dev-box", 8080, nil)
		c, f := newFake(t, existing)

		info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
		if err != nil {
			t.Fatalf("InspectConflict: %v", err)
		}
		if info.Kind != None {
			t.Errorf("Kind = %v, want None (live route already matches the requested backend)", info.Kind)
		}
		if f.mutations != 0 {
			t.Errorf("InspectConflict is read-only, got %d mutations", f.mutations)
		}
	})

	t.Run("backend points elsewhere -> still OwnedDiffBackend", func(t *testing.T) {
		existing := BuildRoute("myapp.example.com", "other-box", 3000, nil)
		c, _ := newFake(t, existing)

		info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
		if err != nil {
			t.Fatalf("InspectConflict: %v", err)
		}
		if info.Kind != OwnedDiffBackend {
			t.Errorf("Kind = %v, want OwnedDiffBackend (backend does not match wanted)", info.Kind)
		}
		if info.Label != "other-box" || info.Port != 3000 {
			t.Errorf("backend = %s:%d, want other-box:3000 (the LIVE holder, not the wanted backend)", info.Label, info.Port)
		}
	})
}

// TestInspectConflictBackendMatchesButForeignCoexists covers roborev 2g50: when
// our owned route already points at the wanted backend, InspectConflict must
// still scan the shared array for ANOTHER overlapping route before declaring the
// conflict resolved -- a coexisting foreign wildcard/exact route would otherwise
// be missed and the caller would PATCH-and-"succeed" while that route can still
// intercept traffic. It must return ForeignOverlap (naming the foreign route),
// not None.
func TestInspectConflictBackendMatchesButForeignCoexists(t *testing.T) {
	// Our owned route already points where we want to publish...
	owned := BuildRoute("myapp.example.com", "dev-box", 8080, nil)
	// ...but a FOREIGN wildcard route (seeded BEFORE it in the array) also
	// overlaps the hostname.
	foreign := Route{
		ID:       "caddy_manual_1",
		Match:    []Match{{Host: []string{"*.example.com"}}},
		Handle:   []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.9:8080"}}}},
		Terminal: true,
	}
	c, f := newFake(t, foreign, owned)

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != ForeignOverlap {
		t.Errorf("Kind = %v, want ForeignOverlap (a foreign wildcard coexists with our matching owned route)", info.Kind)
	}
	if info.ID != "caddy_manual_1" {
		t.Errorf("ID = %q, want the foreign route's id (caddy_manual_1)", info.ID)
	}
	if f.mutations != 0 {
		t.Errorf("InspectConflict is read-only, got %d mutations", f.mutations)
	}
}

// TestInspectConflictIdHijacked is the two-read core (design r2-M1): our @id was
// re-pointed by a foreign edit to a DIFFERENT, non-overlapping host. A plain
// overlap scan for the requested name would miss it (so the old "nothing
// overlaps -> retry" rule livelocks); read 1 catches it via hostMatcherIs and
// classifies IdHijacked, carrying where it now points.
func TestInspectConflictIdHijacked(t *testing.T) {
	stale := BuildRoute("myapp.example.com", "dev-box", 3000, nil)
	stale.Match = []Match{{Host: []string{"evil.example.com"}}} // @id kept, matcher repointed
	c, f := newFake(t, stale)

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != IdHijacked {
		t.Fatalf("Kind = %v, want IdHijacked", info.Kind)
	}
	if !info.Owned || info.ID != "tailport-myapp.example.com" {
		t.Errorf("owned/ID wrong: owned=%v id=%q", info.Owned, info.ID)
	}
	if info.HijackedTo != "evil.example.com" {
		t.Errorf("HijackedTo = %q, want evil.example.com", info.HijackedTo)
	}
	if f.mutations != 0 {
		t.Errorf("InspectConflict is read-only, got %d mutations", f.mutations)
	}
}

// TestInspectConflictForeignOverlap: no @id of ours, a foreign reverse_proxy
// route already claims the host. Read 1 misses (no @id), read 2 finds it ->
// ForeignOverlap naming the foreign backend, Owned=false.
func TestInspectConflictForeignOverlap(t *testing.T) {
	foreign := Route{
		ID:    "someone-elses-route",
		Match: []Match{{Host: []string{"myapp.example.com"}}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}},
		}},
		Terminal: true,
	}
	c, _ := newFake(t, foreign)

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != ForeignOverlap {
		t.Fatalf("Kind = %v, want ForeignOverlap", info.Kind)
	}
	if info.Owned || info.ID != "someone-elses-route" {
		t.Errorf("owned/ID wrong: owned=%v id=%q", info.Owned, info.ID)
	}
	if !info.BackendParseable || info.Label != "192.0.2.9" || info.Port != 8080 {
		t.Errorf("backend wrong: parseable=%v %s:%d", info.BackendParseable, info.Label, info.Port)
	}
}

// TestInspectConflictIdlessForeignOverlap: an id-less foreign route (a hand-
// authored route with no @id) overlapping the host is still ForeignOverlap, with
// ID=="".
func TestInspectConflictIdlessForeignOverlap(t *testing.T) {
	foreign := Route{
		Match: []Match{{Host: []string{"myapp.example.com"}}},
		Handle: []Handler{{
			Handler:   "reverse_proxy",
			Upstreams: []Upstream{{Dial: "10.0.0.1:80"}},
		}},
	}
	c, _ := newFake(t, foreign)

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != ForeignOverlap || info.Owned || info.ID != "" {
		t.Errorf("id-less foreign: kind=%v owned=%v id=%q, want ForeignOverlap/false/\"\"", info.Kind, info.Owned, info.ID)
	}
}

// TestInspectConflictFindsNonProxyForeign is the raw-scan requirement: a foreign
// route with NO reverse_proxy dial (a static_response, seeded by host matcher +
// handler name) is DROPPED by List/parseRoute, yet InspectConflict — which
// matches on the host matcher alone — must still FIND and NAME it. This is why
// the scan is not built on List. (The fake needs no rebuild: classification only
// needs host matcher + @id + handler name, all of which the existing Route/
// Handler decode preserves; the byte-faithful raw-storage rebuild is 6n15's.)
func TestInspectConflictFindsNonProxyForeign(t *testing.T) {
	static := Route{
		ID:       "foreign-static",
		Match:    []Match{{Host: []string{"myapp.example.com"}}},
		Handle:   []Handler{{Handler: "static_response"}}, // no reverse_proxy dial
		Terminal: true,
	}
	c, _ := newFake(t, static)

	// Precondition: List drops it (no parseable backend), proving the scan can't
	// be built on List.
	infos, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("List should skip the non-proxy route, got %+v", infos)
	}

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != ForeignOverlap {
		t.Fatalf("Kind = %v, want ForeignOverlap (non-proxy route must be found)", info.Kind)
	}
	if info.BackendParseable {
		t.Errorf("a static_response has no parseable backend, got BackendParseable=true")
	}
	if info.Handler != "static_response" {
		t.Errorf("Handler = %q, want static_response (names a route the backend can't)", info.Handler)
	}
	if info.ID != "foreign-static" {
		t.Errorf("ID = %q, want foreign-static", info.ID)
	}
}

// TestInspectConflictForeignDisclosable (roborev en3n-#1): a ForeignOverlap is
// Disclosable only when every match block is host-only, so its Hosts list is its
// COMPLETE blast radius. A route matching on more than a hostname (a hostless OR
// block) is NOT disclosable, so the UI can refuse to offer a force-delete for it.
func TestInspectConflictForeignDisclosable(t *testing.T) {
	t.Run("host-only foreign route is disclosable", func(t *testing.T) {
		raw := json.RawMessage(`{"@id":"fp","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.5:80"}]}],"terminal":true}`)
		c, _ := newFakeRaw(t, raw)
		info, err := c.InspectConflict(context.Background(), "app.example.com", "dev-box", 8080)
		if err != nil || info.Kind != ForeignOverlap {
			t.Fatalf("kind=%v err=%v, want ForeignOverlap", info.Kind, err)
		}
		if !info.Disclosable {
			t.Errorf("a host-only foreign route must be Disclosable")
		}
	})
	t.Run("hostless OR block is NOT disclosable", func(t *testing.T) {
		// Same host list, plus a second HOSTLESS block that matches every request.
		raw := json.RawMessage(`{"@id":"fp","match":[{"host":["app.example.com"]},{"path":["/*"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.5:80"}]}],"terminal":true}`)
		c, _ := newFakeRaw(t, raw)
		info, err := c.InspectConflict(context.Background(), "app.example.com", "dev-box", 8080)
		if err != nil || info.Kind != ForeignOverlap {
			t.Fatalf("kind=%v err=%v, want ForeignOverlap", info.Kind, err)
		}
		if info.Disclosable {
			t.Errorf("a route with a hostless OR block must NOT be Disclosable (its Hosts list hides the catch-all)")
		}
	})
}

// TestInspectConflictCaseAndWildcard: the scan overlaps case-insensitively and
// honors the single-label "*.suffix" wildcard, matching Publish's own conflict
// detection.
func TestInspectConflictCaseAndWildcard(t *testing.T) {
	t.Run("case variant", func(t *testing.T) {
		foreign := Route{
			ID:     "case-route",
			Match:  []Match{{Host: []string{"App.Example.COM"}}},
			Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}}}},
		}
		c, _ := newFake(t, foreign)
		info, err := c.InspectConflict(context.Background(), "app.example.com", "dev-box", 8080)
		if err != nil {
			t.Fatalf("InspectConflict: %v", err)
		}
		if info.Kind != ForeignOverlap {
			t.Errorf("case-variant Kind = %v, want ForeignOverlap", info.Kind)
		}
	})

	t.Run("wildcard suffix", func(t *testing.T) {
		foreign := Route{
			ID:     "wildcard-route",
			Match:  []Match{{Host: []string{"*.example.com"}}},
			Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "192.0.2.9:8080"}}}},
		}
		c, _ := newFake(t, foreign)
		info, err := c.InspectConflict(context.Background(), "app.example.com", "dev-box", 8080)
		if err != nil {
			t.Fatalf("InspectConflict: %v", err)
		}
		if info.Kind != ForeignOverlap {
			t.Errorf("wildcard Kind = %v, want ForeignOverlap", info.Kind)
		}
	})
}

// TestInspectConflictNone: nothing overlaps and our @id is clean (the conflict
// cleared between Publish's refusal and this read). Kind=None tells the UI it may
// retry the plain publish once. A present-but-non-overlapping foreign route must
// not be mistaken for a conflict.
func TestInspectConflictNone(t *testing.T) {
	other := BuildRoute("unrelated.example.com", "some-box", 4000, nil)
	c, _ := newFake(t, other)

	info, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if err != nil {
		t.Fatalf("InspectConflict: %v", err)
	}
	if info.Kind != None {
		t.Errorf("Kind = %v, want None (nothing overlaps, @id clean)", info.Kind)
	}
}

// TestInspectConflictUnreachable: a transport failure on either read surfaces as
// ErrUnreachable, not a bogus classification.
func TestInspectConflictUnreachable(t *testing.T) {
	c, _ := newFake(t)
	srv := httptest.NewServer(&fakeAdmin{server: "tailport"})
	closedURL := srv.URL
	srv.Close()
	c.AdminURL = closedURL

	_, err := c.InspectConflict(context.Background(), "myapp.example.com", "dev-box", 8080)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
}

// --- PurgeConflict + capture (kata 6n15) ------------------------------------

// TestPurgeConflictOwnedDeletesByID: an owned conflicting route (our @id) is
// deleted by /id/<id> (stable across index shifts), and the capture carries the
// @id (HadID true).
func TestPurgeConflictOwnedDeletesByID(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil)) // OwnedDiffBackend

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != OwnedDiffBackend {
		t.Fatalf("InspectConflict kind=%v err=%v, want OwnedDiffBackend", info.Kind, err)
	}
	captured, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info))
	if err != nil {
		t.Fatalf("PurgeConflict: %v", err)
	}
	if !captured.HadID {
		t.Errorf("an owned route carries an @id; HadID should be true")
	}
	if captured.Hostname != host {
		t.Errorf("captured hostname = %q, want %q", captured.Hostname, host)
	}
	if rawRouteID(captured.Raw) != IDFor(host) {
		t.Errorf("captured @id = %q, want %q", rawRouteID(captured.Raw), IDFor(host))
	}
	if f.del != 1 || f.mutations != 1 {
		t.Errorf("expected exactly one DELETE; del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.delPaths) != 1 || f.delPaths[0] != "/id/"+IDFor(host) {
		t.Errorf("owned purge must delete by /id; delPaths=%v", f.delPaths)
	}
	if len(f.routes) != 0 {
		t.Errorf("route should be gone; got %d", len(f.routes))
	}
}

// TestPurgeConflictIdlessForeignDeletesByIndex: a truly id-less foreign route is
// deleted by array index under the parent routes-array If-Match, and HadID false.
func TestPurgeConflictIdlessForeignDeletesByIndex(t *testing.T) {
	const host = "app.example.com"
	foreign := Route{
		Match:  []Match{{Host: []string{host}}},
		Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.1:80"}}}},
	}
	c, f := newFake(t, foreign)

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != ForeignOverlap || info.ID != "" {
		t.Fatalf("InspectConflict kind=%v id=%q err=%v, want id-less ForeignOverlap", info.Kind, info.ID, err)
	}
	captured, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info))
	if err != nil {
		t.Fatalf("PurgeConflict: %v", err)
	}
	if captured.HadID {
		t.Errorf("an id-less route has no @id; HadID should be false")
	}
	if f.del != 1 || len(f.delPaths) != 1 || f.delPaths[0] != f.routesPath()+"/0" {
		t.Errorf("id-less foreign purge must delete by index; del=%d delPaths=%v", f.del, f.delPaths)
	}
	if len(f.routes) != 0 {
		t.Errorf("route should be gone; got %d", len(f.routes))
	}
}

// TestPurgeConflictByteFaithfulCapture: an OWNED route seeded raw with a field
// tailport doesn't model round-trips through capture BYTE-FOR-BYTE (this now
// works only because the fake stores raw). A decode+re-marshal would silently
// drop the unmodeled field.
func TestPurgeConflictByteFaithfulCapture(t *testing.T) {
	const host = "app.example.com"
	// Compact, owned, and carrying an unmodeled "metadata" object.
	seed := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true,"metadata":{"note":"hand-edited"}}`)
	c, _ := newFakeRaw(t, seed)

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != OwnedDiffBackend {
		t.Fatalf("InspectConflict kind=%v err=%v, want OwnedDiffBackend", info.Kind, err)
	}
	captured, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info))
	if err != nil {
		t.Fatalf("PurgeConflict: %v", err)
	}
	if string(captured.Raw) != string(seed) {
		t.Errorf("capture is not byte-faithful:\n got %s\nwant %s", captured.Raw, seed)
	}
	// The unmodeled field really is present in the captured bytes.
	if !strings.Contains(string(captured.Raw), `"metadata":{"note":"hand-edited"}`) {
		t.Errorf("captured bytes dropped the unmodeled field: %s", captured.Raw)
	}
}

// TestPurgeConflictNoConflictWhenCleared: the conflict cleared before the purge
// (nothing overlaps any more) → ErrNoConflict, no mutation.
func TestPurgeConflictNoConflictWhenCleared(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil))
	info, _ := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	expect := ExpectFromConflict(info)

	f.mu.Lock()
	f.routes = nil // the holder vanished
	f.mu.Unlock()

	_, err := c.PurgeConflict(context.Background(), host, expect)
	if !errors.Is(err, ErrNoConflict) {
		t.Fatalf("err = %v, want ErrNoConflict", err)
	}
	if f.mutations != 0 {
		t.Errorf("nothing to purge must not mutate; got %d", f.mutations)
	}
}

// TestPurgeConflictOwnedBackendSwapChanged: the owned route's backend was swapped
// between classify and purge (kept our @id + host) → ErrConflictChanged, so the
// UI re-confirms rather than deleting a route pinned to a different backend.
func TestPurgeConflictOwnedBackendSwapChanged(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil))
	info, _ := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	expect := ExpectFromConflict(info) // pins other-box:9090

	swapped, _ := json.Marshal(BuildRoute(host, "sneaky-box", 1234, nil)) // same @id, new backend
	f.mu.Lock()
	f.routes[0] = swapped
	f.mu.Unlock()

	_, err := c.PurgeConflict(context.Background(), host, expect)
	if !errors.Is(err, ErrConflictChanged) {
		t.Fatalf("err = %v, want ErrConflictChanged", err)
	}
	if f.mutations != 0 {
		t.Errorf("a swapped-backend route must not be purged; got %d mutations", f.mutations)
	}
}

// TestPurgeConflictOwnedToForeignChanged is the escalation guard: a route
// approved as OWNED that became FOREIGN (its @id was stripped by a foreign edit)
// must NOT be deleted under the owned confirm → ErrConflictChanged.
func TestPurgeConflictOwnedToForeignChanged(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil))
	info, _ := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	expect := ExpectFromConflict(info) // Owned=true

	foreignified := BuildRoute(host, "other-box", 9090, nil)
	foreignified.ID = "someone-elses-route" // no tailport- prefix → now foreign
	raw, _ := json.Marshal(foreignified)
	f.mu.Lock()
	f.routes[0] = raw
	f.mu.Unlock()

	_, err := c.PurgeConflict(context.Background(), host, expect)
	if !errors.Is(err, ErrConflictChanged) {
		t.Fatalf("err = %v, want ErrConflictChanged (owned→foreign escalation)", err)
	}
	if f.mutations != 0 {
		t.Errorf("an escalated route must not be purged under the owned confirm; got %d mutations", f.mutations)
	}
}

// TestPurgeConflict412ThenSucceedByID proves the /id delete path retries on a 412
// and then succeeds (the successful attempt's If-Match is the id-scope etag the
// fake genuinely validates).
func TestPurgeConflict412ThenSucceedByID(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil))
	info, _ := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	f.force412 = 1 // first DELETE loses the race, the retry wins

	if _, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info)); err != nil {
		t.Fatalf("PurgeConflict should retry and succeed: %v", err)
	}
	if f.force412 != 0 {
		t.Errorf("the injected 412 was not consumed (force412=%d)", f.force412)
	}
	if f.del != 1 || len(f.delPaths) != 1 || f.delPaths[0] != "/id/"+IDFor(host) {
		t.Errorf("expected exactly one accepted /id DELETE after retry; del=%d delPaths=%v", f.del, f.delPaths)
	}
	if len(f.routes) != 0 {
		t.Errorf("route should be gone after the retry; got %d", len(f.routes))
	}
}

// TestPurgeConflict412ThenSucceedByIndex proves the index delete path retries on
// a 412 and then succeeds (the successful attempt carries the parent routes-array
// If-Match, which the fake re-hashes at the embedded path).
func TestPurgeConflict412ThenSucceedByIndex(t *testing.T) {
	const host = "app.example.com"
	foreign := Route{
		Match:  []Match{{Host: []string{host}}},
		Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.1:80"}}}},
	}
	c, f := newFake(t, foreign)
	info, _ := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	f.force412 = 1

	if _, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info)); err != nil {
		t.Fatalf("PurgeConflict should retry and succeed: %v", err)
	}
	if f.force412 != 0 {
		t.Errorf("the injected 412 was not consumed (force412=%d)", f.force412)
	}
	if f.del != 1 || len(f.delPaths) != 1 || f.delPaths[0] != f.routesPath()+"/0" {
		t.Errorf("expected exactly one accepted index DELETE after retry; del=%d delPaths=%v", f.del, f.delPaths)
	}
	if len(f.routes) != 0 {
		t.Errorf("route should be gone after the retry; got %d", len(f.routes))
	}
}

// TestPurgeIndexDeleteParentEtagGuards drives the raw primitives to prove the
// fake genuinely models a PARENT-scope If-Match on an index DELETE: a stale
// routes-array etag is rejected 412 (the array hash moved), a fresh one succeeds.
func TestPurgeIndexDeleteParentEtagGuards(t *testing.T) {
	ctx := context.Background()
	foreign := Route{
		Match:  []Match{{Host: []string{"a.example.com"}}},
		Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.1:80"}}}},
	}
	c, _ := newFake(t, foreign)

	_, e1, err := c.fetchRoutesRaw(ctx) // read the routes-array etag E1
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent editor appends a route with the (still-fresh) E1, moving the
	// array hash off E1.
	extra := Route{ID: "extra", Match: []Match{{Host: []string{"z.example.com"}}}, Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.2:80"}}}}}
	if status, body, err := c.mutate(ctx, http.MethodPost, c.routesURL(), extra, e1); err != nil || status < 200 || status >= 300 {
		t.Fatalf("seed append status=%d body=%s err=%v", status, body, err)
	}

	// DELETE index 0 carrying the now-STALE E1 → 412 (the fake re-hashed the array
	// at the embedded parent path and saw it move).
	_, status, _, err := c.do(ctx, http.MethodDelete, c.routesURL()+"/0", nil, e1)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusPreconditionFailed {
		t.Fatalf("stale-parent-etag index DELETE status = %d, want 412", status)
	}
	// With a fresh array etag the same index DELETE succeeds.
	_, e2, err := c.fetchRoutesRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if e2 == e1 {
		t.Fatalf("the array etag did not move after the append (e1=%q e2=%q)", e1, e2)
	}
	_, status, _, err = c.do(ctx, http.MethodDelete, c.routesURL()+"/0", nil, e2)
	if err != nil {
		t.Fatal(err)
	}
	if status < 200 || status >= 300 {
		t.Fatalf("fresh-parent-etag index DELETE status = %d, want 2xx", status)
	}
}

// TestFakeRejectsDuplicateIDPost: POSTing a route whose @id already exists is
// rejected with a Caddy-style 4xx and nothing is appended (so a purge/capture
// test can't pass for the wrong reason on a blind-append fake).
func TestFakeRejectsDuplicateIDPost(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "dev-box", 8080, nil))
	ctx := context.Background()

	_, arrEtag, err := c.fetchRoutesRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := c.mutate(ctx, http.MethodPost, c.routesURL(), BuildRoute(host, "dev-box", 8080, nil), arrEtag)
	if err != nil {
		t.Fatal(err)
	}
	if status < 400 || status >= 500 {
		t.Fatalf("duplicate @id POST status = %d, want a 4xx", status)
	}
	if !strings.Contains(string(body), "duplicate") {
		t.Errorf("dup-@id body = %q, want it to name the duplicate", body)
	}
	if f.post != 0 || len(f.routes) != 1 {
		t.Errorf("a duplicate @id must not append; post=%d routes=%d", f.post, len(f.routes))
	}
}

// TestPurgeConflictRawHashIdlessForeign exercises the PurgeExpect.RawHash exact-
// bytes identity for an id-less foreign route: an unchanged route purges, a route
// whose bytes drifted (still overlapping) is refused with ErrConflictChanged.
func TestPurgeConflictRawHashIdlessForeign(t *testing.T) {
	const host = "a.example.com"
	seed := json.RawMessage(`{"match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.1:80"}]}]}`)

	t.Run("unchanged purges", func(t *testing.T) {
		c, _ := newFakeRaw(t, seed)
		expect := PurgeExpect{Owned: false, RawHash: rawIdentityHash(seed)}
		captured, err := c.PurgeConflict(context.Background(), host, expect)
		if err != nil {
			t.Fatalf("PurgeConflict: %v", err)
		}
		if string(captured.Raw) != string(seed) {
			t.Errorf("captured %s, want %s", captured.Raw, seed)
		}
	})

	t.Run("drifted refuses", func(t *testing.T) {
		c, f := newFakeRaw(t, seed)
		expect := PurgeExpect{Owned: false, RawHash: rawIdentityHash(seed)}
		f.mu.Lock()
		f.routes[0] = json.RawMessage(`{"match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.9:80"}]}]}`)
		f.mu.Unlock()
		if _, err := c.PurgeConflict(context.Background(), host, expect); !errors.Is(err, ErrConflictChanged) {
			t.Fatalf("err = %v, want ErrConflictChanged", err)
		}
		if f.mutations != 0 {
			t.Errorf("a drifted route must not be purged; got %d mutations", f.mutations)
		}
	})
}

// TestPurgeConflictConcurrentUpdateExhausted: every DELETE loses the If-Match
// race → ErrConcurrentUpdate, nothing deleted.
func TestPurgeConflictConcurrentUpdateExhausted(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil))
	info, _ := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	f.force412 = maxRetries + 2

	_, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info))
	if !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("err = %v, want ErrConcurrentUpdate", err)
	}
	if f.del != 0 || f.mutations != 0 {
		t.Errorf("no delete should have been accepted; del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.routes) != 1 {
		t.Errorf("route must survive an exhausted purge; got %d", len(f.routes))
	}
}

// --- roborev hped hardening: PurgeConflict FIX1 / FIX4 / FIX6 ----------------

// TestPurgeConflictIDRematcherRefused (roborev hped #1): an owned @id re-pointed
// to a DIFFERENT host (same backend) in the array-read→id-read window must NOT be
// deleted. The stale array bytes still carry our identity, so the old code (which
// re-checked expect against the STALE array bytes and never revalidated the live
// matcher) would delete a route the user never approved. The fresh id-read's
// hostMatcherIs catches the drift → ErrConflictChanged, nothing deleted.
func TestPurgeConflictIDRematcherRefused(t *testing.T) {
	const host = "app.example.com"
	c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil)) // OwnedDiffBackend

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != OwnedDiffBackend {
		t.Fatalf("InspectConflict kind=%v err=%v, want OwnedDiffBackend", info.Kind, err)
	}
	expect := ExpectFromConflict(info)

	// Between PurgeConflict's array read and its /id read, a foreign edit re-points
	// our @id at a DIFFERENT host while keeping the backend (id+backend still match).
	f.hookAfterRoutesGet = func() {
		f.routes[0] = json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["evil.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true}`)
	}

	if _, err := c.PurgeConflict(context.Background(), host, expect); !errors.Is(err, ErrConflictChanged) {
		t.Fatalf("err = %v, want ErrConflictChanged (matcher re-pointed under us)", err)
	}
	if f.del != 0 || f.mutations != 0 {
		t.Errorf("a re-pointed @id must not be deleted; del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.routes) != 1 {
		t.Errorf("route must survive; got %d", len(f.routes))
	}
}

// TestPurgeConflictCapturesFreshIDBytes (roborev hped #1): the capture must be the
// bytes ACTUALLY deleted — the fresh /id read — not the stale array bytes. An
// identity-preserving edit (an unmodeled field changes A→B) lands in the
// array-read→id-read window; the purge still commits (matcher + identity stable)
// and the capture carries the FRESH (B) bytes.
func TestPurgeConflictCapturesFreshIDBytes(t *testing.T) {
	const host = "app.example.com"
	seedA := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true,"metadata":{"note":"A"}}`)
	seedB := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true,"metadata":{"note":"B"}}`)
	c, f := newFakeRaw(t, seedA)

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != OwnedDiffBackend {
		t.Fatalf("InspectConflict kind=%v err=%v, want OwnedDiffBackend", info.Kind, err)
	}
	// After the array read, an identity-preserving edit swaps A's unmodeled field
	// for B (same @id, host, backend), so the /id read differs from the array read.
	f.hookAfterRoutesGet = func() { f.routes[0] = seedB }

	captured, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info))
	if err != nil {
		t.Fatalf("PurgeConflict: %v", err)
	}
	if string(captured.Raw) != string(seedB) {
		t.Errorf("capture must equal the FRESH id-read bytes:\n got %s\nwant %s", captured.Raw, seedB)
	}
	if strings.Contains(string(captured.Raw), `"note":"A"`) {
		t.Errorf("capture used the STALE array bytes (note A): %s", captured.Raw)
	}
	if f.del != 1 || len(f.routes) != 0 {
		t.Errorf("the route should be deleted once; del=%d routes=%d", f.del, len(f.routes))
	}
}

// TestPurgeConflictLocatesApprovedNotPrecedingForeign (roborev hped #4): a foreign
// route PRECEDING the owned route, both overlapping the host, must not derail the
// purge. Locating by EXPECT's @id (not first overlap) targets exactly the approved
// owned route — no ErrConflictChanged re-classify loop, and the foreign route
// survives untouched.
func TestPurgeConflictLocatesApprovedNotPrecedingForeign(t *testing.T) {
	const host = "app.example.com"
	id := IDFor(host)
	foreign := Route{
		ID:     "third-party-app",
		Match:  []Match{{Host: []string{host}}}, // overlaps, and PRECEDES the owned route
		Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "3rd:7000"}}}},
	}
	c, f := newFake(t, foreign, BuildRoute(host, "other-box", 9090, nil))

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != OwnedDiffBackend || info.ID != id {
		t.Fatalf("InspectConflict kind=%v id=%q err=%v, want OwnedDiffBackend of our @id", info.Kind, info.ID, err)
	}
	captured, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info))
	if err != nil {
		t.Fatalf("PurgeConflict should delete the owned route, not loop: %v", err)
	}
	if !captured.HadID || rawRouteID(captured.Raw) != id {
		t.Errorf("must capture the OWNED route; hadID=%v id=%q", captured.HadID, rawRouteID(captured.Raw))
	}
	if len(f.delPaths) != 1 || f.delPaths[0] != "/id/"+id {
		t.Errorf("the OWNED route must be deleted by /id; delPaths=%v", f.delPaths)
	}
	if len(f.routes) != 1 || rawRouteID(f.routes[0]) != "third-party-app" {
		t.Errorf("the preceding foreign route must survive; routes=%d", len(f.routes))
	}
}

// TestPurgeConflictIdlessForeignByRawHashDespitePreceding (roborev hped #4): an
// id-less foreign route pinned by RawHash resolves to EXACTLY that route even when
// another overlapping route precedes it — the hash locate ignores position.
func TestPurgeConflictIdlessForeignByRawHashDespitePreceding(t *testing.T) {
	const host = "a.example.com"
	preceding := json.RawMessage(`{"@id":"other","match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.2:80"}]}]}`)
	target := json.RawMessage(`{"match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.1:80"}]}]}`)
	c, f := newFakeRaw(t, preceding, target)

	expect := PurgeExpect{Owned: false, RawHash: rawIdentityHash(target)}
	captured, err := c.PurgeConflict(context.Background(), host, expect)
	if err != nil {
		t.Fatalf("PurgeConflict: %v", err)
	}
	if string(captured.Raw) != string(target) {
		t.Errorf("must capture the hash-pinned target; got %s", captured.Raw)
	}
	if len(f.delPaths) != 1 || f.delPaths[0] != f.routesPath()+"/1" {
		t.Errorf("the target (index 1) must be deleted by index; delPaths=%v", f.delPaths)
	}
	if len(f.routes) != 1 || rawRouteID(f.routes[0]) != "other" {
		t.Errorf("the preceding route must survive; routes=%d", len(f.routes))
	}
}

// TestPurgeConflictRefusesWithoutETag (roborev hped #6): when the edge returns no
// ETag on the read guarding the delete (a pre-2.5.2 edge that ignores If-Match),
// PurgeConflict REFUSES with ErrEdgeNoIfMatch rather than issue an unconditional
// delete a concurrent shift could point at the wrong route — on BOTH the /id path
// and the index path.
func TestPurgeConflictRefusesWithoutETag(t *testing.T) {
	const host = "app.example.com"

	t.Run("id path", func(t *testing.T) {
		c, f := newFake(t, BuildRoute(host, "other-box", 9090, nil))
		f.noEtag = true
		info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
		if err != nil || info.Kind != OwnedDiffBackend {
			t.Fatalf("InspectConflict kind=%v err=%v", info.Kind, err)
		}
		if _, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info)); !errors.Is(err, ErrEdgeNoIfMatch) {
			t.Fatalf("err = %v, want ErrEdgeNoIfMatch", err)
		}
		if f.del != 0 || len(f.routes) != 1 {
			t.Errorf("nothing may be deleted without a guarding etag; del=%d routes=%d", f.del, len(f.routes))
		}
	})

	t.Run("index path", func(t *testing.T) {
		foreign := Route{
			Match:  []Match{{Host: []string{host}}},
			Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.1:80"}}}},
		}
		c, f := newFake(t, foreign)
		f.noEtag = true
		info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
		if err != nil || info.Kind != ForeignOverlap || info.ID != "" {
			t.Fatalf("InspectConflict kind=%v id=%q err=%v", info.Kind, info.ID, err)
		}
		if _, err := c.PurgeConflict(context.Background(), host, ExpectFromConflict(info)); !errors.Is(err, ErrEdgeNoIfMatch) {
			t.Fatalf("err = %v, want ErrEdgeNoIfMatch", err)
		}
		if f.del != 0 || len(f.routes) != 1 {
			t.Errorf("nothing may be deleted without a guarding etag; del=%d routes=%d", f.del, len(f.routes))
		}
	})
}

// TestInspectConflictForeignHostsBlastRadius (roborev hped #3): a foreign wildcard
// / multi-host route carries its FULL host-matcher list on ConflictInfo.Hosts, so
// the UI can disclose the blast radius rather than name only the requested host.
func TestInspectConflictForeignHostsBlastRadius(t *testing.T) {
	const host = "app.example.com"
	foreign := Route{
		ID:     "foreign-multi",
		Match:  []Match{{Host: []string{host, "other.example.com", "*.internal.example.com"}}},
		Handle: []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.5:80"}}}},
	}
	c, _ := newFake(t, foreign)
	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != ForeignOverlap {
		t.Fatalf("InspectConflict kind=%v err=%v, want ForeignOverlap", info.Kind, err)
	}
	want := []string{host, "other.example.com", "*.internal.example.com"}
	if len(info.Hosts) != len(want) {
		t.Fatalf("Hosts = %v, want %v", info.Hosts, want)
	}
	for i, h := range want {
		if info.Hosts[i] != h {
			t.Errorf("Hosts[%d] = %q, want %q", i, info.Hosts[i], h)
		}
	}
}

// --- roborev ve95 hardening: FIX 1 (RawHash through the real flow) / FIX 2 (host
//     set pin) ---------------------------------------------------------------

// TestPurgeConflictIdlessForeignRawHashThroughClassify (roborev ve95 FIX 1)
// exercises the WHOLE production path — InspectConflict → ExpectFromConflict →
// PurgeConflict — for an id-less foreign route, with a STRUCTURALLY-SIMILAR
// sibling (same host, same backend, id-less) inserted AHEAD of the approved route
// after classification. The sibling would be the FIRST overlap PurgeConflict finds
// if the expect fell back to first-overlap; the RawHash computed by InspectConflict
// and carried through ExpectFromConflict must instead re-locate the PRECISE
// approved route, so the SIBLING survives and the APPROVED route is the one
// deleted. It uses no hand-built PurgeExpect — the RawHash is populated only by the
// real classify→expect plumbing FIX 1 adds.
func TestPurgeConflictIdlessForeignRawHashThroughClassify(t *testing.T) {
	const host = "a.example.com"
	// The approved route: id-less foreign, host a.example.com, backend 10.0.0.1:80.
	approved := json.RawMessage(`{"match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.1:80"}]}]}`)
	// A sibling STRUCTURALLY IDENTICAL under the structural re-verify (same host,
	// same backend, id-less) but with DIFFERENT bytes (an unmodeled field), so its
	// RawHash differs. First-overlap would grab this; RawHash must not.
	sibling := json.RawMessage(`{"match":[{"host":["a.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.1:80"}]}],"metadata":{"note":"sibling"}}`)

	c, f := newFakeRaw(t, approved)

	// Classify, then build the re-verify identity exactly as the UI does.
	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != ForeignOverlap || info.ID != "" {
		t.Fatalf("InspectConflict kind=%v id=%q err=%v, want id-less ForeignOverlap", info.Kind, info.ID, err)
	}
	if info.RawHash == "" {
		t.Fatalf("InspectConflict must fingerprint the located id-less route into RawHash (FIX 1)")
	}
	expect := ExpectFromConflict(info)
	if expect.RawHash != info.RawHash {
		t.Fatalf("ExpectFromConflict must carry RawHash for an id-less foreign route (FIX 1); got %q", expect.RawHash)
	}

	// AFTER the user confirmed: a structurally-similar sibling is inserted AHEAD of
	// the approved route (index 0), pushing the approved route to index 1.
	f.mu.Lock()
	f.routes = append([]json.RawMessage{sibling}, f.routes...)
	f.mu.Unlock()

	captured, err := c.PurgeConflict(context.Background(), host, expect)
	if err != nil {
		t.Fatalf("PurgeConflict should re-locate and delete the approved route: %v", err)
	}
	// The APPROVED bytes were captured/deleted — never the sibling's.
	if string(captured.Raw) != string(approved) {
		t.Errorf("captured the wrong route:\n got %s\nwant %s (approved)", captured.Raw, approved)
	}
	if len(f.delPaths) != 1 || f.delPaths[0] != f.routesPath()+"/1" {
		t.Errorf("must delete the approved route at index 1, not the sibling at 0; delPaths=%v", f.delPaths)
	}
	// The sibling must survive untouched.
	if len(f.routes) != 1 || string(f.routes[0]) != string(sibling) {
		t.Errorf("the structurally-similar sibling must survive; routes=%v", f.routes)
	}
}

// TestPurgeConflictForeignIdWidenedMatcherRefused (roborev ve95 FIX 2): a FOREIGN
// route identified by @id whose host matcher is WIDENED (an extra unrelated
// hostname added) between the user's confirm and the delete — while keeping its
// @id and backend — must NOT be deleted with the enlarged blast radius. The
// confirmed host set pinned on PurgeExpect no longer equals the live matcher, so
// the re-verify fails → ErrConflictChanged, nothing deleted, and the UI re-
// classifies to re-disclose the new blast radius.
func TestPurgeConflictForeignIdWidenedMatcherRefused(t *testing.T) {
	const host = "app.example.com"
	foreign := Route{
		ID:       "third-party-app",
		Match:    []Match{{Host: []string{host}}},
		Handle:   []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.5:80"}}}},
		Terminal: true,
	}
	c, f := newFake(t, foreign)

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != ForeignOverlap || info.ID != "third-party-app" {
		t.Fatalf("InspectConflict kind=%v id=%q err=%v, want foreign-with-@id ForeignOverlap", info.Kind, info.ID, err)
	}
	expect := ExpectFromConflict(info)
	if expect.RawHash == "" {
		t.Fatalf("ExpectFromConflict must pin a foreign route by exact bytes (RawHash, cmr5-#1); got empty")
	}

	// Between confirm and purge, a foreign edit WIDENS the matcher (adds an
	// unrelated hostname) while keeping the @id and backend.
	widened := Route{
		ID:       "third-party-app",
		Match:    []Match{{Host: []string{host, "unrelated.example.com"}}},
		Handle:   []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.5:80"}}}},
		Terminal: true,
	}
	raw, _ := json.Marshal(widened)
	f.mu.Lock()
	f.routes[0] = raw
	f.mu.Unlock()

	_, err = c.PurgeConflict(context.Background(), host, expect)
	if !errors.Is(err, ErrConflictChanged) {
		t.Fatalf("err = %v, want ErrConflictChanged (matcher widened under the confirm)", err)
	}
	if f.del != 0 || f.mutations != 0 {
		t.Errorf("a widened-matcher foreign route must not be purged; del=%d mutations=%d", f.del, f.mutations)
	}
	if len(f.routes) != 1 {
		t.Errorf("the route must survive so the UI can re-disclose the blast radius; got %d", len(f.routes))
	}
}

// TestPurgeConflictForeignHostlessOrBlockRefused (roborev cmr5-#1): the case a
// flattened host-SET pin MISSED. A foreign @id route keeps the SAME host list but
// gains a second, HOSTLESS match block (a bare OR term that matches every
// request) between confirm and delete. matcherHostList is unchanged, so the old
// sameHostSet check would have passed and deleted a route now matching all
// traffic. The exact-bytes RawHash pin catches it → ErrConflictChanged.
func TestPurgeConflictForeignHostlessOrBlockRefused(t *testing.T) {
	const host = "app.example.com"
	foreign := Route{
		ID:       "third-party-app",
		Match:    []Match{{Host: []string{host}}},
		Handle:   []Handler{{Handler: "reverse_proxy", Upstreams: []Upstream{{Dial: "10.0.0.5:80"}}}},
		Terminal: true,
	}
	c, f := newFake(t, foreign)

	info, err := c.InspectConflict(context.Background(), host, "dev-box", 8080)
	if err != nil || info.Kind != ForeignOverlap {
		t.Fatalf("InspectConflict kind=%v err=%v, want ForeignOverlap", info.Kind, err)
	}
	expect := ExpectFromConflict(info)

	// A hostless OR match block is added; the host LIST is unchanged, so a
	// host-set comparison would not notice, but the route now matches everything.
	// Build the widened element as raw JSON (a hostless match block isn't
	// expressible via the typed Match host-only builder).
	widenedRaw := json.RawMessage(`{"@id":"third-party-app","match":[{"host":["app.example.com"]},{"path":["/*"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.5:80"}]}],"terminal":true}`)
	f.mu.Lock()
	f.routes[0] = widenedRaw
	f.mu.Unlock()

	if _, err := c.PurgeConflict(context.Background(), host, expect); !errors.Is(err, ErrConflictChanged) {
		t.Fatalf("err = %v, want ErrConflictChanged (a same-host-list matcher widen must be caught by the exact-bytes pin)", err)
	}
	if f.del != 0 || f.mutations != 0 {
		t.Errorf("a widened foreign route must not be deleted; del=%d mutations=%d", f.del, f.mutations)
	}
}

// --- RestoreRoute / VerifyRestore (kata ttfh, the OWNED-only undo) -----------

// capturedOwned is a compact, owned captured route carrying an UNMODELED
// "metadata" field, so restore/verify are exercised against a route that a
// decode+re-marshal would mutilate. It points app.example.com at other-box:9090.
const capturedOwned = `{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true,"metadata":{"note":"hand-edited"}}`

// TestRestoreRouteAppendsWhenFree: with the name genuinely free, RestoreRoute
// POST-appends the EXACT captured bytes (byte-faithful, unmodeled field intact).
func TestRestoreRouteAppendsWhenFree(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned)
	c, f := newFakeRaw(t) // empty array: nothing claims the host
	if err := c.RestoreRoute(context.Background(), host, captured); err != nil {
		t.Fatalf("RestoreRoute: %v", err)
	}
	if f.post != 1 || f.mutations != 1 || len(f.routes) != 1 {
		t.Fatalf("expected exactly one appended route; post=%d mutations=%d routes=%d", f.post, f.mutations, len(f.routes))
	}
	if string(f.routes[0]) != string(captured) {
		t.Errorf("restore is not byte-faithful:\n got %s\nwant %s", f.routes[0], captured)
	}
	if !strings.Contains(string(f.routes[0]), `"metadata":{"note":"hand-edited"}`) {
		t.Errorf("restore dropped the unmodeled field: %s", f.routes[0])
	}
}

// TestRestoreRouteRefusesOverlappingClaim: an overlapping (foreign) route already
// holds the host → refuse with ErrRestoreNameClaimed, never a blind append that
// would create dual exposure.
func TestRestoreRouteRefusesOverlappingClaim(t *testing.T) {
	const host = "app.example.com"
	claim := json.RawMessage(`{"@id":"foreign-app","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.9:80"}]}],"terminal":true}`)
	c, f := newFakeRaw(t, claim)
	err := c.RestoreRoute(context.Background(), host, json.RawMessage(capturedOwned))
	if !errors.Is(err, ErrRestoreNameClaimed) {
		t.Fatalf("err = %v, want ErrRestoreNameClaimed", err)
	}
	if f.mutations != 0 {
		t.Errorf("a re-claimed name must not be appended (dual exposure); got %d mutations", f.mutations)
	}
}

// TestRestoreRouteRefusesWildcardOverlap: a foreign *.suffix wildcard that would
// intercept the host is an overlap → refuse (the scan reuses routeOverlaps).
func TestRestoreRouteRefusesWildcardOverlap(t *testing.T) {
	const host = "app.example.com"
	wild := json.RawMessage(`{"@id":"foreign-wild","match":[{"host":["*.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true}`)
	c, f := newFakeRaw(t, wild)
	err := c.RestoreRoute(context.Background(), host, json.RawMessage(capturedOwned))
	if !errors.Is(err, ErrRestoreNameClaimed) {
		t.Fatalf("err = %v, want ErrRestoreNameClaimed (wildcard overlap)", err)
	}
	if f.mutations != 0 {
		t.Errorf("a wildcard-claimed name must not be appended; got %d mutations", f.mutations)
	}
}

// TestRestoreRouteRefusesDuplicateID: a route carrying the captured @id but at a
// DIFFERENT (non-overlapping) host — the overlap scan misses it, the @id scan
// catches it → refuse (a duplicate @id would be rejected by Caddy anyway).
func TestRestoreRouteRefusesDuplicateID(t *testing.T) {
	const host = "app.example.com"
	dup := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["elsewhere.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"x:1"}]}],"terminal":true}`)
	c, f := newFakeRaw(t, dup)
	err := c.RestoreRoute(context.Background(), host, json.RawMessage(capturedOwned))
	if !errors.Is(err, ErrRestoreNameClaimed) {
		t.Fatalf("err = %v, want ErrRestoreNameClaimed (duplicate @id)", err)
	}
	if f.mutations != 0 {
		t.Errorf("a duplicate @id must not be appended; got %d mutations", f.mutations)
	}
}

// TestRestoreRoute412Retries: the array moved under the append (a lost If-Match
// race) → 412 → re-read, re-scan, retry, then succeed exactly once.
func TestRestoreRoute412Retries(t *testing.T) {
	const host = "app.example.com"
	c, f := newFakeRaw(t)
	f.force412 = 1
	if err := c.RestoreRoute(context.Background(), host, json.RawMessage(capturedOwned)); err != nil {
		t.Fatalf("RestoreRoute should retry and succeed: %v", err)
	}
	if f.force412 != 0 {
		t.Errorf("the injected 412 was not consumed (force412=%d)", f.force412)
	}
	if f.post != 1 || f.mutations != 1 || len(f.routes) != 1 {
		t.Errorf("expected exactly one accepted append after retry; post=%d mutations=%d routes=%d", f.post, f.mutations, len(f.routes))
	}
}

// TestRestoreRouteEmptyEtagRefuses: a pre-2.5.2 edge emits no ETag, so the append
// would be unconditional (racing a concurrent claim) → ErrEdgeNoIfMatch, no
// mutation.
func TestRestoreRouteEmptyEtagRefuses(t *testing.T) {
	const host = "app.example.com"
	c, f := newFakeRaw(t)
	f.noEtag = true
	err := c.RestoreRoute(context.Background(), host, json.RawMessage(capturedOwned))
	if !errors.Is(err, ErrEdgeNoIfMatch) {
		t.Fatalf("err = %v, want ErrEdgeNoIfMatch", err)
	}
	if f.mutations != 0 {
		t.Errorf("no ETag must refuse to append, not clobber blind; got %d mutations", f.mutations)
	}
}

// TestVerifyRestoreRestored: exactly one route overlaps and it semantically
// equals the captured bytes (proved with a KEY-REORDERED live element, so it is a
// normalized-JSON compare, not a byte compare) → Restored.
func TestVerifyRestoreRestored(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned)
	// Same values, keys reordered and whitespace added: byte-different, value-equal.
	reordered := json.RawMessage(`{ "terminal": true, "@id":"tailport-app.example.com", "metadata":{"note":"hand-edited"}, "match":[{"host":["app.example.com"]}], "handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}] }`)
	c, _ := newFakeRaw(t, reordered)
	state, err := c.VerifyRestore(context.Background(), host, captured)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if state != Restored {
		t.Errorf("state = %v, want Restored (normalized compare of reordered-but-equal route)", state)
	}
}

// TestVerifyRestoreUnclaimed: nothing overlaps the host → Unclaimed.
func TestVerifyRestoreUnclaimed(t *testing.T) {
	c, _ := newFakeRaw(t)
	state, err := c.VerifyRestore(context.Background(), "app.example.com", json.RawMessage(capturedOwned))
	if err != nil || state != RestoreUnclaimed {
		t.Fatalf("state=%v err=%v, want Unclaimed", state, err)
	}
}

// TestVerifyRestoreClaimedByOther: a DIFFERENT route (foreign @id) holds the host
// → ClaimedByOther, never "restored".
func TestVerifyRestoreClaimedByOther(t *testing.T) {
	const host = "app.example.com"
	other := json.RawMessage(`{"@id":"foreign-app","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"10.0.0.9:80"}]}],"terminal":true}`)
	c, _ := newFakeRaw(t, other)
	state, err := c.VerifyRestore(context.Background(), host, json.RawMessage(capturedOwned))
	if err != nil || state != RestoreClaimedByOther {
		t.Fatalf("state=%v err=%v, want ClaimedByOther", state, err)
	}
}

// TestVerifyRestoreConcurrentOverlapAppend (roborev-k7br-#2): a concurrent
// overlapping route appended between B and C means >1 route overlaps — even
// though one of them equals captured, the name is not cleanly ours → ClaimedByOther,
// NOT "restored".
func TestVerifyRestoreConcurrentOverlapAppend(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned)
	concurrent := json.RawMessage(`{"@id":"foreign-late","match":[{"host":["app.example.com"]}],"handle":[{"handler":"static_response"}],"terminal":true}`)
	c, _ := newFakeRaw(t, captured, concurrent) // our restored route AND a concurrent overlap
	state, err := c.VerifyRestore(context.Background(), host, captured)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if state != RestoreClaimedByOther {
		t.Errorf("state = %v, want ClaimedByOther (a second overlapping route → not 'restored')", state)
	}
}

// TestVerifyRestoreContentMismatchBackend (roborev-e4ne-#1): a same-@id PATCH
// between B and C keeps the id+host but swaps the backend → ContentMismatch, not
// "restored".
func TestVerifyRestoreContentMismatchBackend(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned) // other-box:9090
	swapped := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"sneaky-box:1111"}]}],"terminal":true,"metadata":{"note":"hand-edited"}}`)
	c, _ := newFakeRaw(t, swapped)
	state, err := c.VerifyRestore(context.Background(), host, captured)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if state != RestoreContentMismatch {
		t.Errorf("state = %v, want ContentMismatch (same @id, swapped backend)", state)
	}
}

// TestVerifyRestoreContentMismatchUnmodeledField: the content compare includes
// fields tailport doesn't model — a same-@id route whose only change is an
// UNMODELED field is still ContentMismatch, proving the compare is semantic-full
// (not hostMatcherIs / backend-only).
func TestVerifyRestoreContentMismatchUnmodeledField(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned) // metadata note "hand-edited"
	mutatedMeta := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true,"metadata":{"note":"TAMPERED"}}`)
	c, _ := newFakeRaw(t, mutatedMeta)
	state, err := c.VerifyRestore(context.Background(), host, captured)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if state != RestoreContentMismatch {
		t.Errorf("state = %v, want ContentMismatch (unmodeled field changed under same @id)", state)
	}
}

// TestVerifyRestoreIDMovedOffHostname (kata 7jy2 FIX 2): the captured route's @id
// was concurrently re-pointed to a DIFFERENT host between B and C, so NOTHING
// overlaps the original hostname — but our restored route still exists (mutated)
// elsewhere. A pure overlap scan would report Unclaimed, hiding it; the full-array
// @id scan must catch it → ContentMismatch, not Unclaimed.
func TestVerifyRestoreIDMovedOffHostname(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned) // @id tailport-app.example.com, host app.example.com
	// Same @id, but its host matcher re-pointed to a non-overlapping host: no route
	// overlaps app.example.com, yet the captured @id is present with mutated content.
	moved := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["elsewhere.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"other-box:9090"}]}],"terminal":true,"metadata":{"note":"hand-edited"}}`)
	c, _ := newFakeRaw(t, moved)
	state, err := c.VerifyRestore(context.Background(), host, captured)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if state != RestoreContentMismatch {
		t.Errorf("state = %v, want ContentMismatch (captured @id re-pointed off the hostname, not Unclaimed)", state)
	}
}

// TestSemanticallyEqualPreservesLargeIntegers (kata 7jy2 FIX 3): two routes that
// differ ONLY in an integer field above 2^53 must NOT compare equal. A plain
// interface{} decode routes JSON numbers through float64, which collapses 2^53 and
// 2^53+1 to one value → a false "restored". Decoding with UseNumber() (json.Number
// preserves the exact digits) keeps them distinct.
func TestSemanticallyEqualPreservesLargeIntegers(t *testing.T) {
	// 9007199254740992 == 2^53; 9007199254740993 == 2^53+1 (indistinguishable as float64).
	a := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy"}],"big":9007199254740992}`)
	b := json.RawMessage(`{"@id":"tailport-app.example.com","match":[{"host":["app.example.com"]}],"handle":[{"handler":"reverse_proxy"}],"big":9007199254740993}`)
	if semanticallyEqual(a, b) {
		t.Error("distinct integers above 2^53 must NOT be semanticallyEqual (json.Number precision)")
	}
	// Sanity: genuinely identical bytes still compare equal.
	if !semanticallyEqual(a, json.RawMessage(string(a))) {
		t.Error("identical routes must compare equal")
	}
}

// TestRestoreThenVerifyComposes: RestoreRoute into a free array then VerifyRestore
// classifies Restored — the B→C composition proves out end-to-end against the fake.
func TestRestoreThenVerifyComposes(t *testing.T) {
	const host = "app.example.com"
	captured := json.RawMessage(capturedOwned)
	c, _ := newFakeRaw(t)
	if err := c.RestoreRoute(context.Background(), host, captured); err != nil {
		t.Fatalf("RestoreRoute: %v", err)
	}
	state, err := c.VerifyRestore(context.Background(), host, captured)
	if err != nil || state != Restored {
		t.Fatalf("state=%v err=%v, want Restored", state, err)
	}
}
