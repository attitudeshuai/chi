package chi

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
)

// newDiagRouter builds a router exercising every branch kind: static, param,
// regexp, wildcard and two levels of mounted subrouters.
//
//	/                         root
//	/login                    POST            (static leaf)
//	/about                    GET             (static leaf)
//	/wild/{name}              GET             (param)
//	/files/{f}.html           GET             (param with '.' tail)
//	/api/*                    -> v1
//	  /users/{id}             GET
//	  /v2/*                   -> v2
//	    /things               POST
//	    /things/{id:[0-9]+}   GET
//
// Optional custom miss handlers are installed on every level (so they also
// run inside mounted routers), and optional middlewares on the root.
func newDiagRouter(notFound, methodNotAllowed http.HandlerFunc, mws ...func(http.Handler) http.Handler) *Mux {
	body := func(text string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(text))
		}
	}

	v2 := NewRouter()
	v1 := NewRouter()
	root := NewRouter()

	for _, mw := range mws {
		root.Use(mw)
	}
	if notFound != nil {
		root.NotFound(notFound)
		v1.NotFound(notFound)
		v2.NotFound(notFound)
	}
	if methodNotAllowed != nil {
		root.MethodNotAllowed(methodNotAllowed)
		v1.MethodNotAllowed(methodNotAllowed)
		v2.MethodNotAllowed(methodNotAllowed)
	}

	v2.Post("/things", body("things-post"))
	v2.Get("/things/{id:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("thing:" + URLParam(r, "id")))
	})

	v1.Get("/users/{id}", body("user"))
	v1.Mount("/v2", v2)

	root.Get("/about", body("about"))
	root.Post("/login", body("login"))
	root.Get("/wild/{name}", body("wild"))
	root.Get("/files/{f}.html", body("file"))
	root.Mount("/api", v1)
	return root
}

// matchDiag runs one programmatic match with diagnostics enabled.
func matchDiag(t *testing.T, mx *Mux, method, path string) *RouteDiagnostics {
	t.Helper()
	rctx := NewRouteContext()
	rctx.EnableRouteDiagnostics()
	mx.Match(rctx, method, path)
	return rctx.RouteDiagnostics()
}

func attemptReasons(d *RouteDiagnostics) []RouteDiagReason {
	if d == nil {
		return nil
	}
	out := make([]RouteDiagReason, 0, len(d.Attempts))
	for _, a := range d.Attempts {
		out = append(out, a.Reason)
	}
	return out
}

func hasReason(d *RouteDiagnostics, reason RouteDiagReason) bool {
	return slices.Contains(attemptReasons(d), reason)
}

func attemptsAt(d *RouteDiagnostics, level int) []RouteDiagAttempt {
	var out []RouteDiagAttempt
	for _, a := range d.Attempts {
		if a.Level == level {
			out = append(out, a)
		}
	}
	return out
}

// TestDiagReasonsDistinguishable verifies the five required failure classes
// are produced and distinguishable, plus the overall 404/405 separation and
// the allowed-methods attachment on 405.
func TestDiagReasonsDistinguishable(t *testing.T) {
	mx := newDiagRouter(nil, nil)

	t.Run("prefix_mismatch on static", func(t *testing.T) {
		d := matchDiag(t, mx, "GET", "/abut")
		if d == nil || d.MethodNotAllowed {
			t.Fatalf("expected a 404 diagnostic, got %+v", d)
		}
		if !hasReason(d, RouteDiagPrefixMismatch) {
			t.Fatalf("expected prefix_mismatch attempt, got %+v", d.Attempts)
		}
	})

	t.Run("param_empty", func(t *testing.T) {
		d := matchDiag(t, mx, "GET", "/api/users/")
		if d == nil || d.MethodNotAllowed {
			t.Fatalf("expected a 404 diagnostic, got %+v", d)
		}
		if !hasReason(d, RouteDiagParamEmpty) {
			t.Fatalf("expected param_empty attempt, got %+v", d.Attempts)
		}
	})

	t.Run("param_mismatch on delimiter/shape", func(t *testing.T) {
		// {f} requires a closing '.', absent here.
		d := matchDiag(t, mx, "GET", "/files/no-dot")
		if d == nil {
			t.Fatal("expected diagnostics")
		}
		if !hasReason(d, RouteDiagParamMismatch) {
			t.Fatalf("expected param_mismatch attempt, got %+v", d.Attempts)
		}
		// a value crossing a '/' boundary is the same shape of failure
		d = matchDiag(t, mx, "GET", "/files/a/b.html")
		if !hasReason(d, RouteDiagParamMismatch) {
			t.Fatalf("expected param_mismatch attempt for cross-slash value, got %+v", d.Attempts)
		}
	})

	t.Run("regexp_mismatch", func(t *testing.T) {
		d := matchDiag(t, mx, "GET", "/api/v2/things/abc")
		if d == nil || d.MethodNotAllowed {
			t.Fatalf("expected a 404 diagnostic, got %+v", d)
		}
		if !hasReason(d, RouteDiagRegexpMismatch) {
			t.Fatalf("expected regexp_mismatch attempt, got %+v", d.Attempts)
		}
	})

	t.Run("method_not_allowed with allowed methods", func(t *testing.T) {
		d := matchDiag(t, mx, "DELETE", "/login")
		if d == nil || !d.MethodNotAllowed {
			t.Fatalf("expected a 405 diagnostic, got %+v", d)
		}
		if !hasReason(d, RouteDiagMethodNotAllowed) {
			t.Fatalf("expected method_not_allowed attempt, got %+v", d.Attempts)
		}
		if got, want := d.AllowedMethods, []string{"POST"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("allowed methods = %v, want %v", got, want)
		}
		var found bool
		for _, a := range attemptsAt(d, 0) {
			if a.Reason != RouteDiagMethodNotAllowed {
				continue
			}
			found = true
			if !reflect.DeepEqual(a.AllowedMethods, []string{"POST"}) {
				t.Fatalf("attempt allowed methods = %v, want [POST]", a.AllowedMethods)
			}
			if a.Pattern != "/login" {
				t.Fatalf("attempt pattern = %q, want /login", a.Pattern)
			}
		}
		if !found {
			t.Fatal("missing method_not_allowed attempt")
		}
	})

	t.Run("method_not_allowed inside mounted subrouter", func(t *testing.T) {
		d := matchDiag(t, mx, "DELETE", "/api/v2/things/7")
		if d == nil || !d.MethodNotAllowed {
			t.Fatalf("expected a 405 diagnostic, got %+v", d)
		}
		if !hasReason(d, RouteDiagMethodNotAllowed) {
			t.Fatalf("expected method_not_allowed attempt, got %+v", d.Attempts)
		}
		if !slices.Equal(d.AllowedMethods, []string{"GET"}) {
			t.Fatalf("allowed methods = %v, want [GET]", d.AllowedMethods)
		}
	})

	t.Run("wildcard_uncovered through mount chain", func(t *testing.T) {
		d := matchDiag(t, mx, "GET", "/api/v2/ghost")
		if d == nil || d.MethodNotAllowed {
			t.Fatalf("expected a 404 diagnostic, got %+v", d)
		}
		if !hasReason(d, RouteDiagWildcardUncovered) {
			t.Fatalf("expected wildcard_uncovered attempt, got %+v", d.Attempts)
		}
	})

	t.Run("no_route after a matched param segment", func(t *testing.T) {
		d := matchDiag(t, mx, "GET", "/wild/a/b")
		if d == nil {
			t.Fatal("expected diagnostics")
		}
		if !hasReason(d, RouteDiagNoRoute) {
			t.Fatalf("expected no_route attempt, got %+v", d.Attempts)
		}
	})

	t.Run("404 and 405 are reported separately", func(t *testing.T) {
		notFound := matchDiag(t, mx, "GET", "/nowhere")
		if notFound == nil || notFound.MethodNotAllowed || len(notFound.AllowedMethods) != 0 {
			t.Fatalf("path miss must report a plain 404, got %+v", notFound)
		}
		notAllowed := matchDiag(t, mx, "PUT", "/login")
		if notAllowed == nil || !notAllowed.MethodNotAllowed {
			t.Fatalf("method miss must report a 405, got %+v", notAllowed)
		}
	})
}

// TestDiagMatchedAndDisabled is the opt-in/no-surprise contract: disabled
// contexts never expose diagnostics; enabled contexts hide them on a hit.
func TestDiagMatchedAndDisabled(t *testing.T) {
	mx := newDiagRouter(nil, nil)

	// Disabled: nothing collected, nothing exposed.
	rctx := NewRouteContext()
	mx.Match(rctx, "GET", "/nowhere")
	if d := rctx.RouteDiagnostics(); d != nil {
		t.Fatalf("disabled context must expose no diagnostics, got %+v", d)
	}
	if rctx.routeDiag != nil {
		t.Fatal("disabled context must not allocate a collector")
	}

	// Enabled but matched: no report.
	if d := matchDiag(t, mx, "GET", "/about"); d != nil {
		t.Fatalf("matched request must expose no diagnostics, got %+v", d)
	}
	if d := matchDiag(t, mx, "GET", "/api/v2/things/7"); d != nil {
		t.Fatalf("matched mounted request must expose no diagnostics, got %+v", d)
	}
}

// TestDiagMountLevels verifies merged, level-tagged, stable ordering across
// nested mounts.
func TestDiagMountLevels(t *testing.T) {
	mx := newDiagRouter(nil, nil)

	d := matchDiag(t, mx, "GET", "/api/v2/ghost")
	if d == nil {
		t.Fatal("expected diagnostics")
	}

	// Levels must appear in non-decreasing order (attempt order across the
	// mount stack: root wildcard delegation, then subrouter attempts).
	prev := 0
	for i, a := range d.Attempts {
		if i > 0 && a.Level < prev {
			t.Fatalf("attempt levels out of order at %d: %+v", i, d.Attempts)
		}
		prev = a.Level
	}

	// Each mount level contributes its own wildcard_uncovered entry, tagged
	// with the pattern of that mount point.
	l0 := attemptsAt(d, 0)
	if len(l0) == 0 || l0[0].Reason != RouteDiagWildcardUncovered || l0[0].Pattern != "/api/*" {
		t.Fatalf("level 0 must open with the /api/* wildcard, got %+v", l0)
	}
	var sawV1 bool
	for _, a := range attemptsAt(d, 1) {
		if a.Reason == RouteDiagWildcardUncovered && a.Pattern == "/v2/*" {
			sawV1 = true
		}
	}
	if !sawV1 {
		t.Fatalf("level 1 must record the /v2/* wildcard, got %+v", attemptsAt(d, 1))
	}
	l2 := attemptsAt(d, 2)
	if len(l2) == 0 {
		t.Fatal("level 2 must contain the leaf-level failure attempts")
	}
	if !hasReason(&RouteDiagnostics{Attempts: l2}, RouteDiagPrefixMismatch) {
		t.Fatalf("level 2 must explain the leaf failure, got %+v", l2)
	}

	// Repeated requests yield identical diagnostics in the same order.
	var first *RouteDiagnostics
	for i := 0; i < 30; i++ {
		cur := matchDiag(t, mx, "GET", "/api/v2/ghost")
		if first == nil {
			first = cur
			continue
		}
		if !reflect.DeepEqual(first, cur) {
			t.Fatalf("diagnostics not stable on iteration %d:\nfirst=%+v\ncur  =%+v", i, first, cur)
		}
	}
}

// TestDiagDeterministicAcrossMisses repeats every kind of miss and compares
// full reports for stability.
func TestDiagDeterministicAcrossMisses(t *testing.T) {
	mx := newDiagRouter(nil, nil)
	cases := []struct{ m, p string }{
		{"GET", "/abut"},
		{"GET", "/api/users/"},
		{"GET", "/files/no-dot"},
		{"GET", "/files/a/b.html"},
		{"GET", "/api/v2/things/abc"},
		{"DELETE", "/login"},
		{"DELETE", "/api/v2/things/7"},
		{"GET", "/api/v2/ghost"},
		{"GET", "/wild/a/b"},
		{"GET", "/nowhere"},
	}
	for _, c := range cases {
		var first *RouteDiagnostics
		for i := 0; i < 20; i++ {
			cur := matchDiag(t, mx, c.m, c.p)
			if first == nil {
				first = cur
			} else if !reflect.DeepEqual(first, cur) {
				t.Fatalf("%s %s unstable at %d:\n%+v\n%+v", c.m, c.p, i, first, cur)
			}
		}
	}
}

// captureMissHandlers returns NotFound/MethodNotAllowed handlers that keep a
// copy of the diagnostic but still emit the stock responses.
func captureMissHandlers(t *testing.T, mu *sync.Mutex, reports map[string]*RouteDiagnostics) (http.HandlerFunc, http.HandlerFunc) {
	t.Helper()
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := RouteContext(r.Context()).RouteDiagnostics(); d != nil {
			mu.Lock()
			reports["404:"+r.URL.Path] = d
			mu.Unlock()
		}
		http.NotFound(w, r)
	})
	mna := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := RouteContext(r.Context())
		if d := rctx.RouteDiagnostics(); d != nil {
			mu.Lock()
			reports["405:"+r.URL.Path] = d
			mu.Unlock()
			for _, name := range d.AllowedMethods {
				w.Header().Add("Allow", name)
			}
		} else {
			for _, mt := range rctx.methodsAllowed {
				w.Header().Add("Allow", reverseMethodMap[mt])
			}
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	return notFound, mna
}

// TestDiagResponseIdenticalOnOff proves diagnostics never alter the externally
// visible response, on hits and misses, including through mounted routers.
func TestDiagResponseIdenticalOnOff(t *testing.T) {
	var (
		mu      sync.Mutex
		reports = map[string]*RouteDiagnostics{}
	)
	nf, mna := captureMissHandlers(t, &mu, reports)

	// Opt-in middleware; enabled per request via a header.
	diagMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Route-Diag") == "1" {
				RouteContext(r.Context()).EnableRouteDiagnostics()
			}
			next.ServeHTTP(w, r)
		})
	}

	mx := newDiagRouter(nf, mna, diagMW)
	ts := httptest.NewServer(mx)
	defer ts.Close()

	do := func(method, path string, diag bool) (int, string, []string) {
		req, err := http.NewRequest(method, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if diag {
			req.Header.Set("X-Route-Diag", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		allow := append([]string{}, resp.Header.Values("Allow")...)
		slices.Sort(allow)
		return resp.StatusCode, string(b), allow
	}

	battery := []struct{ m, p string }{
		{"GET", "/about"},
		{"GET", "/api/v2/things/7"},
		{"POST", "/api/v2/things"},
		{"GET", "/missing"},
		{"GET", "/api/users/"},
		{"GET", "/api/v2/things/abc"},
		{"GET", "/api/v2/ghost"},
		{"DELETE", "/login"},
		{"DELETE", "/api/v2/things/7"},
		{"GET", "/wild/a/b"},
		{"DIE", "/about"}, // method unsupported by chi itself -> 405, empty Allow
	}
	for _, c := range battery {
		offCode, offBody, offAllow := do(c.m, c.p, false)
		onCode, onBody, onAllow := do(c.m, c.p, true)
		if offCode != onCode || offBody != onBody || !slices.Equal(offAllow, onAllow) {
			t.Fatalf("response differs for %s %s:\noff = %d %q %v\non  = %d %q %v",
				c.m, c.p, offCode, offBody, offAllow, onCode, onBody, onAllow)
		}
		// A second enabled request must be identical to the first (stability
		// over the pooled, reset routing contexts).
		onCode2, onBody2, onAllow2 := do(c.m, c.p, true)
		if onCode != onCode2 || onBody != onBody2 || !slices.Equal(onAllow, onAllow2) {
			t.Fatalf("response not repeatable for %s %s", c.m, c.p)
		}
	}

	// Misses observed while serving must have been captured, hits must not.
	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{
		"404:/missing", "404:/api/v2/ghost", "405:/login", "405:/api/v2/things/7",
	} {
		if reports[key] == nil {
			t.Fatalf("expected captured diagnostic for %s, got none", key)
		}
	}
	for _, key := range []string{"404:/about", "404:/api/v2/things/7"} {
		if _, ok := reports[key]; ok {
			t.Fatalf("matched request %s must not be captured", key)
		}
	}
}

// TestDiagSnapshotDetached ensures a retrieved report survives pooling and
// reuse of the routing context without mutating.
func TestDiagSnapshotDetached(t *testing.T) {
	mx := newDiagRouter(nil, nil)

	rctx := NewRouteContext()
	rctx.EnableRouteDiagnostics()
	mx.Match(rctx, "DELETE", "/login")
	d1 := rctx.RouteDiagnostics()
	if d1 == nil {
		t.Fatal("expected a report")
	}

	// Reuse the same context object (as the pool does).
	rctx.Reset()
	rctx.EnableRouteDiagnostics()
	mx.Match(rctx, "GET", "/api/v2/ghost")
	d2 := rctx.RouteDiagnostics()

	if !d1.MethodNotAllowed || !slices.Equal(d1.AllowedMethods, []string{"POST"}) {
		t.Fatalf("first report changed: %+v", d1)
	}
	if d2 == nil || d2.MethodNotAllowed {
		t.Fatalf("second report wrong: %+v", d2)
	}
	if !hasReason(d1, RouteDiagMethodNotAllowed) || !hasReason(d2, RouteDiagWildcardUncovered) {
		t.Fatalf("reports leaked into each other: %+v %+v", d1, d2)
	}
}

// TestDiagCloneDetachesCollector checks Clone keeps an independent copy.
func TestDiagCloneDetachesCollector(t *testing.T) {
	mx := newDiagRouter(nil, nil)
	rctx := NewRouteContext()
	rctx.EnableRouteDiagnostics()
	mx.Match(rctx, "DELETE", "/login")
	clone := rctx.Clone()

	d1 := rctx.RouteDiagnostics()
	d2 := clone.RouteDiagnostics()
	if !reflect.DeepEqual(d1, d2) {
		t.Fatalf("cloned diagnostics differ:\n%+v\n%+v", d1, d2)
	}

	// Mutating the live context's next run must not touch the clone.
	rctx.Reset()
	rctx.EnableRouteDiagnostics()
	mx.Match(rctx, "GET", "/nowhere")
	if d3 := clone.RouteDiagnostics(); !reflect.DeepEqual(d3, d2) {
		t.Fatalf("clone changed after reuse:\n%+v\n%+v", d3, d2)
	}
}

// TestDiagEmptyMountedSubrouter covers a mounted router that never had any
// routes registered: its decision still surfaces the parent wildcard.
func TestDiagEmptyMountedSubrouter(t *testing.T) {
	var (
		mu      sync.Mutex
		reports []*RouteDiagnostics
	)
	empty := NewRouter() // no routes, handler chain never built
	root := NewRouter()
	root.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			RouteContext(r.Context()).EnableRouteDiagnostics()
			next.ServeHTTP(w, r)
		})
	})
	root.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if d := RouteContext(r.Context()).RouteDiagnostics(); d != nil {
			mu.Lock()
			reports = append(reports, d)
			mu.Unlock()
		}
		http.NotFound(w, r)
	})
	root.Mount("/empty", empty)

	ts := httptest.NewServer(root)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/empty/x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("captured %d reports, want 1", len(reports))
	}
	d := reports[0]
	if d.MethodNotAllowed {
		t.Fatal("empty mount miss must be a 404, not 405")
	}
	var saw bool
	for _, a := range d.Attempts {
		if a.Level == 0 && a.Reason == RouteDiagWildcardUncovered && a.Pattern == "/empty/*" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("missing parent wildcard entry: %+v", d.Attempts)
	}
}

// TestDiagConcurrentProgrammatic hammers Match from many goroutines with
// private contexts and distinct requests, asserting no cross-request bleed.
func TestDiagConcurrentProgrammatic(t *testing.T) {
	mx := newDiagRouter(nil, nil)

	const goroutines = 64
	const iterations = 100

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Alternate request shapes with distinct signatures and a
				// matched request that must always report nil.
				var (
					wantMNA bool
					wantOne RouteDiagReason
				)
				method, path := "GET", "/api/v2/ghost"
				switch g % 4 {
				case 0:
					wantOne = RouteDiagRegexpMismatch
					method, path = "GET", "/api/v2/things/abc"
				case 1:
					wantMNA = true
					wantOne = RouteDiagMethodNotAllowed
					method, path = "DELETE", "/login"
				case 2:
					wantOne = RouteDiagParamEmpty
					method, path = "GET", "/api/users/"
				case 3:
					// hit path: diagnostics must be nil every time
					rctx := NewRouteContext()
					rctx.EnableRouteDiagnostics()
					if !mx.Match(rctx, "GET", "/about") || rctx.RouteDiagnostics() != nil {
						errs <- fmt.Errorf("hit request leaked diagnostics")
						return
					}
					continue
				}

				rctx := NewRouteContext()
				rctx.EnableRouteDiagnostics()
				mx.Match(rctx, method, path)
				d := rctx.RouteDiagnostics()
				if d == nil {
					errs <- fmt.Errorf("g%d i%d: missing report", g, i)
					return
				}
				if d.MethodNotAllowed != wantMNA {
					errs <- fmt.Errorf("g%d i%d: outcome=%v want %v", g, i, d.MethodNotAllowed, wantMNA)
					return
				}
				if !hasReason(d, wantOne) {
					errs <- fmt.Errorf("g%d i%d: missing reason %s in %+v", g, i, wantOne, d.Attempts)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestDiagConcurrentServer verifies isolation over the real pooled request
// lifecycle: concurrent misses capture their own reports without bleed.
func TestDiagConcurrentServer(t *testing.T) {
	var reports sync.Map // request path -> *RouteDiagnostics
	nf := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := RouteContext(r.Context()).RouteDiagnostics(); d != nil {
			reports.Store(r.URL.Path, d)
		}
		http.NotFound(w, r)
	})
	enableMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			RouteContext(r.Context()).EnableRouteDiagnostics()
			next.ServeHTTP(w, r)
		})
	}
	mx := newDiagRouter(nf, nil, enableMW)

	ts := httptest.NewServer(mx)
	defer ts.Close()

	const goroutines = 32
	const iterations = 50

	signatures := []struct {
		pathFormat string
		reason     RouteDiagReason
	}{
		{"/api/job/%d/ghost", RouteDiagWildcardUncovered},
		{"/api/v2/things/abc%d", RouteDiagRegexpMismatch},
	}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			sig := signatures[g%len(signatures)]
			for i := 0; i < iterations; i++ {
				path := fmt.Sprintf(sig.pathFormat, g)
				resp, err := http.Get(ts.URL + path)
				if err != nil {
					errs <- err
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 404 {
					errs <- fmt.Errorf("%s: status %d", path, resp.StatusCode)
					return
				}
				v, ok := reports.LoadAndDelete(path)
				if !ok {
					errs <- fmt.Errorf("%s: no report captured", path)
					return
				}
				d := v.(*RouteDiagnostics)
				if !hasReason(d, sig.reason) {
					errs <- fmt.Errorf("%s: want reason %s in %+v", path, sig.reason, d.Attempts)
					return
				}
				// Levels may only belong to root plus the two mounts; any
				// other value indicates a foreign request leaked in.
				for _, a := range d.Attempts {
					if a.Level < 0 || a.Level > 2 {
						errs <- fmt.Errorf("%s: foreign level %d", path, a.Level)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
