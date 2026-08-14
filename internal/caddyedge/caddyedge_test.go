package caddyedge

import (
	"context"
	"encoding/json"
	"errors"
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
		{"a.b.example.com", true},   // nested subdomains allowed
		{"deep.a.b.example.com", true},
		{"host", true},              // single label is a valid hostname
		{"x-y.example.com", true},   // internal hyphen ok
		{"", false},                 // empty
		{"has space.com", false},    // whitespace
		{"http://x.com", false},     // scheme
		{"x.com/path", false},       // path
		{"x.example.com:8080", false}, // port
		{".example.com", false},     // leading dot
		{"example.com.", false},     // trailing dot
		{"a..b.com", false},         // doubled dot -> empty label
		{"-bad.com", false},         // label starts with hyphen
		{"bad-.com", false},         // label ends with hyphen
		{"under_score.com", false},  // underscore not LDH
	}
	for _, c := range cases {
		if got := ValidHostname(c.in); got != c.ok {
			t.Errorf("ValidHostname(%q) = %v, want %v", c.in, got, c.ok)
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
// uses. It models /id/<id> (GET/PATCH/DELETE), the routes array (GET/POST),
// Etag emission, If-Match -> 412 on mismatch, and — crucially — records whether
// any client mutation actually occurred, so "no mutation" assertions are real.
type fakeAdmin struct {
	mu      sync.Mutex
	server  string  // server name embedded in the routes path
	routes  []Route // the shared routes array
	version int     // drives the Etag; every accepted mutation bumps it

	// force412, when > 0, injects a precondition failure on each of the next
	// N client mutations (simulating a concurrent edit that also bumps the
	// Etag), letting tests exercise the 412 re-read/retry loop deterministically.
	force412 int
	// deleteReturns404, when true, makes DELETE answer 404 without removing the
	// route, to exercise the "404 on DELETE = success" branch.
	deleteReturns404 bool

	// Recorded, accepted client mutations (a rejected 412 is NOT counted).
	mutations int
	post      int
	patch     int
	del       int
	put       int // any PUT attempt at all — the code must never issue one
}

func (f *fakeAdmin) etag() string { return `"` + strconv.Itoa(f.version) + `"` }

func (f *fakeAdmin) indexOf(id string) int {
	for i, r := range f.routes {
		if r.ID == id {
			return i
		}
	}
	return -1
}

// precheck enforces optimistic concurrency for a mutation. It returns false
// (having written a 412 response) when a forced failure is pending or the
// client's If-Match doesn't match the current Etag.
func (f *fakeAdmin) precheck(w http.ResponseWriter, r *http.Request) bool {
	if f.force412 > 0 {
		f.force412--
		f.version++ // a concurrent editor moved the config
		w.Header().Set("Etag", f.etag())
		http.Error(w, "config changed under you", http.StatusPreconditionFailed)
		return false
	}
	if im := r.Header.Get("If-Match"); im != "" && im != f.etag() {
		w.Header().Set("Etag", f.etag())
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
	case r.URL.Path == "/config/apps/http/servers/"+f.server+"/routes":
		f.handleRoutes(w, r)
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
		w.Header().Set("Etag", f.etag())
		f.writeJSON(w, f.routes[idx])
	case http.MethodPatch:
		if !f.precheck(w, r) {
			return
		}
		if idx < 0 {
			http.Error(w, "unknown object id", http.StatusNotFound)
			return
		}
		var nr Route
		if err := json.NewDecoder(r.Body).Decode(&nr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.routes[idx] = nr
		f.version++
		f.mutations++
		f.patch++
		w.Header().Set("Etag", f.etag())
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
		f.version++
		f.mutations++
		f.del++
		w.Header().Set("Etag", f.etag())
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
		w.Header().Set("Etag", f.etag())
		f.writeJSON(w, f.routes)
	case http.MethodPost:
		if !f.precheck(w, r) {
			return
		}
		var nr Route
		if err := json.NewDecoder(r.Body).Decode(&nr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.routes = append(f.routes, nr)
		f.version++
		f.mutations++
		f.post++
		w.Header().Set("Etag", f.etag())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// newFake starts a fakeAdmin httptest.Server seeded with routes and returns a
// Client wired to it plus the fake (for mutation assertions).
func newFake(t *testing.T, routes ...Route) (*Client, *fakeAdmin) {
	t.Helper()
	f := &fakeAdmin{server: "tailport", routes: routes}
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
	if len(f.routes) != 1 || f.routes[0].ID != "tailport-myapp.example.com" {
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
	if len(f.routes) != 1 || f.routes[0].Match[0].Host[0] != "evil.example.com" {
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

// TestHostsOverlap locks the overlap predicate directly, including the
// requested-wildcard direction and the single-label boundary (a "*.suffix"
// wildcard matches exactly one deeper label, not two).
func TestHostsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"app.example.com", "app.example.com", true},    // exact
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
