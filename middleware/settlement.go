package middleware

// Per-request settlement ("final accounting") support.
//
// A request is settled exactly once, with a single SettlementReason. The same
// Settlement is carried through the request context, so every layer of the
// chain (including mounted subrouters) observes the same outcome and a later
// layer can never overwrite an earlier reason. The feature is strictly
// opt-in: middlewares that only know the plain Logger/RequestLogger behave
// exactly as before, and all the marking helpers below are no-ops when the
// request carries no Settlement.

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
)

// SettlementReason is the terminal reason of a settled request. The zero
// value ("") means no reason can be determined yet.
type SettlementReason string

const (
	// ReasonCompleted means the handler chain finished normally ("完成").
	ReasonCompleted SettlementReason = "completed"

	// ReasonDisconnected means the client went away before the response
	// completed ("客户端提前断开").
	ReasonDisconnected SettlementReason = "client-disconnected"

	// ReasonDeadline means a request deadline, such as the one enforced by
	// the Timeout middleware, was reached ("截止时间到").
	ReasonDeadline SettlementReason = "deadline-exceeded"

	// ReasonPanic means the handler chain panicked while processing the
	// request ("处理中崩溃").
	ReasonPanic SettlementReason = "panic"

	// ReasonAborted means the chain deliberately aborted the request, for
	// example by panicking with http.ErrAbortHandler ("被链路提前中止").
	ReasonAborted SettlementReason = "aborted"

	// ReasonUnmatched means no route matched the request ("未匹配").
	ReasonUnmatched SettlementReason = "unmatched"
)

// SettlementCtxKey is the context.Context key to store the per-request
// Settlement.
var SettlementCtxKey = &contextKey{"Settlement"}

// Settlement is the single settlement result shared by every layer of a
// request chain. Its reason is first-wins: the first reason that can be
// determined sticks and later attempts do not override it.
//
// A Settlement is safe for concurrent use because the context it lives in
// can be canceled concurrently with the handler returning.
type Settlement struct {
	mu     sync.Mutex
	reason SettlementReason
}

// Reason returns the settled reason, or "" when no reason has been
// determined yet.
func (s *Settlement) Reason() SettlementReason {
	s.mu.Lock()
	r := s.reason
	s.mu.Unlock()
	return r
}

// Mark records reason unless a reason was already recorded. It reports
// whether reason was accepted, so callers can tell that an earlier layer
// won the settlement.
func (s *Settlement) Mark(reason SettlementReason) bool {
	if reason == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" {
		return false
	}
	s.reason = reason
	return true
}

// reset clears the settlement so a recycled object can never carry the
// reason of a previous request.
func (s *Settlement) reset() {
	s.mu.Lock()
	s.reason = ""
	s.mu.Unlock()
}

// Settlements are pooled to avoid allocating one per request. Both acquire
// and release reset the object, so a recycled Settlement is always clean.
var settlementPool = sync.Pool{New: func() any { return new(Settlement) }}

func acquireSettlement() *Settlement {
	s := settlementPool.Get().(*Settlement)
	s.reset()
	return s
}

func releaseSettlement(s *Settlement) {
	s.reset()
	settlementPool.Put(s)
}

// GetSettlement returns the Settlement shared by the request chain, or nil
// when settlement was not enabled for the request.
func GetSettlement(ctx context.Context) *Settlement {
	s, _ := ctx.Value(SettlementCtxKey).(*Settlement)
	return s
}

// WithSettlement stores s on the request context. The returned request
// shares the settlement with every handler further down the chain.
func WithSettlement(r *http.Request, s *Settlement) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), SettlementCtxKey, s))
}

// markSettlement records reason on the request's shared Settlement, if any.
// It is a no-op for requests that did not opt into settlement.
func markSettlement(ctx context.Context, reason SettlementReason) {
	if s := GetSettlement(ctx); s != nil {
		s.Mark(reason)
	}
}

// watch marks the settlement as soon as ctx is done: a deadline reaching
// the end is ReasonDeadline, any other cancellation (typically the client
// going away) is ReasonDisconnected. The reason is therefore readable
// *before* the handler returns, rather than only while finalizing.
//
// The returned stop function blocks until the watcher goroutine has exited,
// so a Settlement can be returned to the pool without the watcher still
// touching it.
func (s *Settlement) watch(ctx context.Context) (stop func()) {
	stopCh := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.Mark(ReasonDeadline)
			} else {
				s.Mark(ReasonDisconnected)
			}
		case <-stopCh:
		}
	}()
	return func() {
		close(stopCh)
		<-exited
	}
}

// watchRequestSettlement starts watch on ctx when the request carries a
// Settlement. It returns a no-op stop otherwise.
func watchRequestSettlement(ctx context.Context, r *http.Request) func() {
	s := GetSettlement(r.Context())
	if s == nil {
		return func() {}
	}
	return s.watch(ctx)
}

// finalize settles the request once. A reason already recorded by an inner
// layer (disconnect, deadline, panic, abort) always wins; otherwise an
// unmatched route settles as ReasonUnmatched and a served request as
// ReasonCompleted.
func finalizeSettlement(s *Settlement, r *http.Request) {
	if s.Reason() != "" {
		return
	}
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.Missed() {
		s.Mark(ReasonUnmatched)
		return
	}
	s.Mark(ReasonCompleted)
}

// Settle opts a request chain into per-request settlement.
//
// The outermost Settle layer creates the single Settlement and settles the
// request once; additional Settle layers, for example on mounted
// subrouters, find the existing Settlement, share it, and neither settle
// nor finalize again.
//
// Client disconnects and reached deadlines are marked as soon as they
// happen, so handlers can read GetSettlement(r.Context()).Reason() before
// they return. SettlingRequestLogger already settles requests on its own;
// Settle is useful when settlement is wanted programmatically without that
// logger (or together with a custom LogFormatter).
func Settle(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// An outer layer already owns this request's settlement: share it.
		if GetSettlement(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}

		s := acquireSettlement()
		r = WithSettlement(r, s)
		stop := s.watch(r.Context())

		defer func() {
			stop()
			if rvr := recover(); rvr != nil {
				// No Recoverer handled this: record the outcome, then keep
				// the panic propagating so existing behavior is unchanged.
				if rvr == http.ErrAbortHandler {
					s.Mark(ReasonAborted)
				} else {
					s.Mark(ReasonPanic)
				}
				releaseSettlement(s)
				panic(rvr)
			}
			finalizeSettlement(s, r)
			releaseSettlement(s)
		}()

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
