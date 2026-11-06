package middleware

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- test doubles -----------------------------------------------------------

type settlementRecord struct {
	method string
	uri    string
	status int
	bytes  int
	extra  any
}

type captureSettlementFormatter struct {
	mu      sync.Mutex
	creates int
	records []settlementRecord
}

type captureSettlementEntry struct {
	req *http.Request
	f   *captureSettlementFormatter
}

func (f *captureSettlementFormatter) NewLogEntry(r *http.Request) LogEntry {
	f.mu.Lock()
	f.creates++
	f.mu.Unlock()
	return &captureSettlementEntry{req: r, f: f}
}

func (e *captureSettlementEntry) Write(status, bytes int, _ http.Header, _ time.Duration, extra any) {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	e.f.records = append(e.f.records, settlementRecord{
		method: e.req.Method,
		uri:    e.req.RequestURI,
		status: status,
		bytes:  bytes,
		extra:  extra,
	})
}

func (e *captureSettlementEntry) Panic(any, []byte) {}

func (f *captureSettlementFormatter) snapshot() []settlementRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]settlementRecord, len(f.records))
	copy(out, f.records)
	return out
}

func (f *captureSettlementFormatter) waitFor(t *testing.T, n int) []settlementRecord {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.records)
		f.mu.Unlock()
		if got >= n {
			return f.snapshot()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("settlement records: want %d, got %d", n, len(f.snapshot()))
	return nil
}

func recordReason(t *testing.T, rec settlementRecord) SettlementReason {
	t.Helper()
	se, ok := rec.extra.(SettlementExtra)
	if !ok {
		t.Fatalf("record for %s: extra is %T, want SettlementExtra", rec.uri, rec.extra)
	}
	return se.Reason
}

// pollReason waits up to a second for the settlement to carry want.
func pollReason(t *testing.T, s *Settlement, want SettlementReason) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.Reason() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("settlement reason: want %q, got %q", want, s.Reason())
}

// silenceRecoverer redirects the panic stack trace away from test output.
func silenceRecoverer(t *testing.T) {
	t.Helper()
	prev := recovererErrorWriter
	recovererErrorWriter = io.Discard
	t.Cleanup(func() { recovererErrorWriter = prev })
}

// ---- unit tests -------------------------------------------------------------

func TestSettlementFirstWinsAndPoolReset(t *testing.T) {
	s := acquireSettlement()
	if s.Reason() != "" {
		t.Fatalf("fresh settlement reason = %q, want empty", s.Reason())
	}
	if !s.Mark(ReasonPanic) {
		t.Fatal("first Mark must be accepted")
	}
	if s.Mark(ReasonCompleted) {
		t.Fatal("later Mark must not override the first reason")
	}
	if got := s.Reason(); got != ReasonPanic {
		t.Fatalf("reason = %q, want %q", got, ReasonPanic)
	}

	releaseSettlement(s)
	s2 := acquireSettlement()
	if got := s2.Reason(); got != "" {
		t.Fatalf("recycled settlement carries previous reason %q", got)
	}
	releaseSettlement(s2)
}

// ---- acceptance: four request kinds, exactly one record each ----------------

func TestSettlementCompletedRecord(t *testing.T) {
	capFmt := &captureSettlementFormatter{}
	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Get("/hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/hello", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	recs := capFmt.waitFor(t, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if got := recordReason(t, recs[0]); got != ReasonCompleted {
		t.Fatalf("reason = %q, want %q", got, ReasonCompleted)
	}
	if recs[0].status != 200 || recs[0].bytes != 5 {
		t.Fatalf("status/bytes = %d/%d, want 200/5", recs[0].status, recs[0].bytes)
	}
}

func TestSettlementPanicSingleRecord(t *testing.T) {
	silenceRecoverer(t)
	capFmt := &captureSettlementFormatter{}
	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Use(Recoverer)
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	recs := capFmt.waitFor(t, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(recs))
	}
	if got := recordReason(t, recs[0]); got != ReasonPanic {
		t.Fatalf("reason = %q, want %q", got, ReasonPanic)
	}
}

func TestSettlementAbortedSingleRecord(t *testing.T) {
	capFmt := &captureSettlementFormatter{}
	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Use(Recoverer)
	r.Get("/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })

	defer func() {
		rvr := recover()
		if rvr != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler to keep propagating", rvr)
		}
		recs := capFmt.waitFor(t, 1)
		if len(recs) != 1 {
			t.Fatalf("records = %d, want exactly 1", len(recs))
		}
		if got := recordReason(t, recs[0]); got != ReasonAborted {
			t.Fatalf("reason = %q, want %q", got, ReasonAborted)
		}
	}()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/abort", nil))
	t.Fatal("expected ErrAbortHandler to propagate")
}

func TestSettlementUnmatchedAnd405(t *testing.T) {
	capFmt := &captureSettlementFormatter{}
	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/users/42", nil))

	recs := capFmt.waitFor(t, 2)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if recs[0].status != http.StatusNotFound || recordReason(t, recs[0]) != ReasonUnmatched {
		t.Fatalf("404 case: status=%d reason=%q", recs[0].status, recordReason(t, recs[0]))
	}
	// 405 means the path matched: it is a normal completion, not a miss.
	if recs[1].status != http.StatusMethodNotAllowed || recordReason(t, recs[1]) != ReasonCompleted {
		t.Fatalf("405 case: status=%d reason=%q", recs[1].status, recordReason(t, recs[1]))
	}
}

func TestSettlementTimeoutReadableBeforeReturn(t *testing.T) {
	capFmt := &captureSettlementFormatter{}
	inHandler := make(chan SettlementReason, 1)

	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Use(Timeout(50 * time.Millisecond))
	r.Get("/slow", func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
		s := GetSettlement(req.Context())
		if s == nil {
			t.Error("no settlement in handler context")
			return
		}
		pollReason(t, s, ReasonDeadline)
		inHandler <- s.Reason()
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Fatalf("504 body = %q, want empty (rejection text unchanged)", string(body))
	}

	select {
	case got := <-inHandler:
		if got != ReasonDeadline {
			t.Fatalf("in-handler reason = %q, want %q", got, ReasonDeadline)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed the deadline reason")
	}

	recs := capFmt.waitFor(t, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(recs))
	}
	if got := recordReason(t, recs[0]); got != ReasonDeadline {
		t.Fatalf("record reason = %q, want %q", got, ReasonDeadline)
	}
	if recs[0].status != http.StatusGatewayTimeout {
		t.Fatalf("record status = %d, want 504", recs[0].status)
	}
}

func TestSettlementDisconnectReadableBeforeReturn(t *testing.T) {
	capFmt := &captureSettlementFormatter{}
	started := make(chan struct{})
	inHandler := make(chan SettlementReason, 1)

	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Get("/hang", func(_ http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
		s := GetSettlement(req.Context())
		if s == nil {
			t.Error("no settlement in handler context")
			return
		}
		pollReason(t, s, ReasonDisconnected)
		inHandler <- s.Reason()
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/hang", nil)
	go func() {
		<-started
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected a client-side transport error after cancel")
	}

	select {
	case got := <-inHandler:
		if got != ReasonDisconnected {
			t.Fatalf("in-handler reason = %q, want %q", got, ReasonDisconnected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed the disconnect reason")
	}

	recs := capFmt.waitFor(t, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(recs))
	}
	if got := recordReason(t, recs[0]); got != ReasonDisconnected {
		t.Fatalf("record reason = %q, want %q", got, ReasonDisconnected)
	}
}

func TestSettlementThrottleQueuedCancel(t *testing.T) {
	capFmt := &captureSettlementFormatter{}
	releaseHold := make(chan struct{})
	holdStarted := make(chan struct{})

	r := chi.NewRouter()
	r.Use(SettlingRequestLogger(capFmt))
	r.Use(ThrottleWithOpts(ThrottleOpts{
		Limit: 1, BacklogLimit: 1, BacklogTimeout: time.Minute,
	}))
	r.Get("/hold", func(http.ResponseWriter, *http.Request) {
		close(holdStarted)
		<-releaseHold
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	// Occupy the only processing token.
	holdDone := make(chan struct{})
	go func() {
		resp, err := http.Get(ts.URL + "/hold")
		if err == nil {
			resp.Body.Close()
		}
		close(holdDone)
	}()
	<-holdStarted

	// Second request lands in the backlog; cancel it while queued.
	ctx, cancel := context.WithCancel(context.Background())
	queued, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/hold", nil)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := http.DefaultClient.Do(queued); err == nil {
		t.Fatal("expected queued request to fail after client cancel")
	}

	recs := capFmt.waitFor(t, 1)
	if got := recordReason(t, recs[0]); got != ReasonDisconnected {
		t.Fatalf("queued cancel reason = %q, want %q", got, ReasonDisconnected)
	}

	close(releaseHold)
	<-holdDone
}

// ---- acceptance: reason is independent of mount depth ----------------------

// settlingApp builds an app whose same endpoint set is nested depth levels
// deep, with a SettlingRequestLogger mounted on EVERY router level. Every
// scenario must still produce exactly one record with the same reason.
func settlingApp(t *testing.T, depth int) (*chi.Mux, *captureSettlementFormatter) {
	t.Helper()
	silenceRecoverer(t)
	capFmt := &captureSettlementFormatter{}

	root := chi.NewRouter()
	root.Use(SettlingRequestLogger(capFmt))

	var current chi.Router = root
	for i := 0; i < depth; i++ {
		next := chi.NewRouter()
		next.Use(SettlingRequestLogger(capFmt))
		current.Mount("/a", next)
		current = next
	}
	current.Use(Recoverer)
	current.Get("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	current.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	return root, capFmt
}

func TestSettlementReasonIndependentOfMountDepth(t *testing.T) {
	for _, depth := range []int{0, 1, 3} {
		t.Run("", func(t *testing.T) {
			r, capFmt := settlingApp(t, depth)
			ts := httptest.NewServer(r)
			defer ts.Close()

			pfx := ""
			for i := 0; i < depth; i++ {
				pfx += "/a"
			}

			// normal completion
			resp, err := http.Get(ts.URL + pfx + "/ok")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			// panic
			resp, err = http.Get(ts.URL + pfx + "/boom")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			// unmatched, also inside the nested chain
			resp, err = http.Get(ts.URL + pfx + "/missing")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			recs := capFmt.waitFor(t, 3)
			if len(recs) != 3 {
				t.Fatalf("depth=%d records=%d, want 3 (one per request)", depth, len(recs))
			}
			want := []SettlementReason{ReasonCompleted, ReasonPanic, ReasonUnmatched}
			for i, w := range want {
				if got := recordReason(t, recs[i]); got != w {
					t.Fatalf("depth=%d record %d reason=%q, want %q", depth, i, got, w)
				}
			}
		})
	}
}

// ---- acceptance: feature off stays byte-identical ---------------------------

var elapsedRE = regexp.MustCompile(` in \S+`)

func serveOnce(t *testing.T, h http.Handler) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
}

func TestSettlementDisabledIsByteIdentical(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	var oldOut, newOut bytes.Buffer
	oldH := RequestLogger(&DefaultLogFormatter{Logger: log.New(&oldOut, "", 0), NoColor: true})(handler)
	newH := SettlingRequestLogger(&DefaultLogFormatter{Logger: log.New(&newOut, "", 0), NoColor: true})(handler)

	serveOnce(t, oldH)
	serveOnce(t, newH)

	oldLine := elapsedRE.ReplaceAllString(oldOut.String(), " in ELAPSED")
	newLine := elapsedRE.ReplaceAllString(newOut.String(), " in ELAPSED")
	if oldLine != newLine {
		t.Fatalf("output differs with the new capability off:\nold: %q\nnew: %q", oldLine, newLine)
	}
	if bytes.Contains(oldOut.Bytes(), []byte("reason=")) || bytes.Contains(newOut.Bytes(), []byte("reason=")) {
		t.Fatal("reason must not be rendered when WithReason is false")
	}
}

func TestSettlementEnabledRendersReason(t *testing.T) {
	var out bytes.Buffer
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})
	h := SettlingRequestLogger(&DefaultLogFormatter{
		Logger:     log.New(&out, "", 0),
		NoColor:    true,
		WithReason: true,
	})(handler)
	serveOnce(t, h)

	line := elapsedRE.ReplaceAllString(out.String(), " in ELAPSED")
	want := `"GET http://example.com/x HTTP/1.1" from 192.0.2.1:1234 - 200 5B in ELAPSED reason=completed` + "\n"
	if line != want {
		t.Fatalf("line = %q, want %q", line, want)
	}
}

// ---- acceptance: concurrent stress, no reason cross-talk --------------------

func TestSettlementConcurrentStress(t *testing.T) {
	silenceRecoverer(t)
	capFmt := &captureSettlementFormatter{}

	var rawEntries int64
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			atomic.AddInt64(&rawEntries, 1)
			next.ServeHTTP(w, req)
		})
	})
	r.Use(SettlingRequestLogger(capFmt))
	r.Use(Recoverer)
	r.Get("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })

	ts := httptest.NewServer(r)
	defer ts.Close()

	const workers = 40
	const iterations = 10
	const total = workers * iterations * 3

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 10 * time.Second}
			paths := []string{"/ok", "/boom", "/missing"}
			for i := 0; i < iterations; i++ {
				for _, path := range paths {
					resp, err := client.Get(ts.URL + path)
					if err != nil {
						t.Errorf("request %s: %v", path, err)
						return
					}
					io.Copy(io.Discard, resp.Body)
					switch path {
					case "/ok":
						if resp.StatusCode != http.StatusOK {
							t.Errorf("%s status = %d", path, resp.StatusCode)
						}
					case "/boom":
						if resp.StatusCode != http.StatusInternalServerError {
							t.Errorf("%s status = %d", path, resp.StatusCode)
						}
					case "/missing":
						if resp.StatusCode != http.StatusNotFound {
							t.Errorf("%s status = %d", path, resp.StatusCode)
						}
					}
					resp.Body.Close()
				}
			}
		}()
	}
	wg.Wait()

	recs := capFmt.waitFor(t, total)
	if len(recs) != total {
		byURI := map[string]int{}
		for _, rc := range recs {
			byURI[rc.uri]++
		}
		t.Fatalf("records = %d, want exactly %d (creates=%d rawEntries=%d byURI=%v)",
			len(recs), total, capFmt.creates, atomic.LoadInt64(&rawEntries), byURI)
	}
	if got := atomic.LoadInt64(&rawEntries); got != total {
		t.Fatalf("raw middleware entries = %d, want %d", got, total)
	}
	if capFmt.creates != total {
		t.Fatalf("log entries created = %d, want %d", capFmt.creates, total)
	}

	counts := map[SettlementReason]int{}
	for _, rec := range recs {
		got := recordReason(t, rec)
		wantByPath := map[string]SettlementReason{
			"/ok": ReasonCompleted, "/boom": ReasonPanic, "/missing": ReasonUnmatched,
		}
		want, ok := wantByPath[rec.uri]
		if !ok {
			t.Errorf("unexpected record uri %q", rec.uri)
			continue
		}
		if got != want {
			t.Errorf("record %s: reason = %q, want %q (cross-request leak?)", rec.uri, got, want)
		}
		counts[got]++
	}
	if len(counts) != 3 || counts[ReasonCompleted] != workers*iterations ||
		counts[ReasonPanic] != workers*iterations || counts[ReasonUnmatched] != workers*iterations {
		t.Fatalf("reason counts = %v", counts)
	}
}

// ---- Settle middleware standalone -------------------------------------------

func TestSettleMiddlewareProvidesSettlement(t *testing.T) {
	saw := make(chan *Settlement, 1)
	r := chi.NewRouter()
	r.Use(Settle)
	r.Get("/", func(_ http.ResponseWriter, req *http.Request) {
		saw <- GetSettlement(req.Context())
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	s := <-saw
	if s == nil {
		t.Fatal("Settle did not put a settlement in the request context")
	}
}

func TestSettleMiddlewareDisconnect(t *testing.T) {
	started := make(chan struct{})
	inHandler := make(chan SettlementReason, 1)

	r := chi.NewRouter()
	r.Use(Settle)
	r.Get("/hang", func(_ http.ResponseWriter, req *http.Request) {
		close(started)
		<-req.Context().Done()
		s := GetSettlement(req.Context())
		pollReason(t, s, ReasonDisconnected)
		inHandler <- s.Reason()
	})

	ts := httptest.NewServer(r)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/hang", nil)
	go func() {
		<-started
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected transport error after cancel")
	}
	select {
	case got := <-inHandler:
		if got != ReasonDisconnected {
			t.Fatalf("reason = %q, want %q", got, ReasonDisconnected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed the reason")
	}
}
