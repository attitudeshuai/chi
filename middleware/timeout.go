package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Timeout is a middleware that cancels ctx after a given timeout and return
// a 504 Gateway Timeout error to the client.
//
// It's required that you select the ctx.Done() channel to check for the signal
// if the context has reached its deadline and return, otherwise the timeout
// signal will be just ignored.
//
// ie. a route/handler may look like:
//
//	r.Get("/long", func(w http.ResponseWriter, r *http.Request) {
//		ctx := r.Context()
//		processTime := time.Duration(rand.Intn(4)+1) * time.Second
//
//		select {
//		case <-ctx.Done():
//			return
//
//		case <-time.After(processTime):
//			// The above channel simulates some hard work.
//		}
//
//		w.Write([]byte("done"))
//	})
func Timeout(timeout time.Duration) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			// Surface a reached deadline (or an earlier client cancel) on
			// the shared settlement as soon as it happens, so the reason is
			// readable before the handler returns rather than only here.
			stop := watchRequestSettlement(ctx, r)
			defer func() {
				stop()
				// Read the error before cancel(): canceling a deadline
				// context that finished normally turns its error into
				// context.Canceled.
				ctxErr := ctx.Err()
				cancel()
				switch {
				case errors.Is(ctxErr, context.DeadlineExceeded):
					markSettlement(r.Context(), ReasonDeadline)
					w.WriteHeader(http.StatusGatewayTimeout)
				case errors.Is(ctxErr, context.Canceled):
					// The parent request was canceled, typically by the
					// client going away, before the timeout elapsed.
					markSettlement(r.Context(), ReasonDisconnected)
				}
			}()

			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)
		}
		return http.HandlerFunc(fn)
	}
}
