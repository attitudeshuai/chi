package middleware

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"
)

var (
	// LogEntryCtxKey is the context.Context key to store the request log entry.
	LogEntryCtxKey = &contextKey{"LogEntry"}

	// DefaultLogger is called by the Logger middleware handler to log each request.
	// Its made a package-level variable so that it can be reconfigured for custom
	// logging configurations.
	DefaultLogger func(next http.Handler) http.Handler
)

// Logger is a middleware that logs the start and end of each request, along
// with some useful data about what was requested, what the response status was,
// and how long it took to return. When standard output is a TTY, Logger will
// print in color, otherwise it will print in black and white. Logger prints a
// request ID if one is provided.
//
// Alternatively, look at https://github.com/goware/httplog for a more in-depth
// http logger with structured logging support.
//
// IMPORTANT NOTE: Logger should go before any other middleware that may change
// the response, such as middleware.Recoverer. Example:
//
//	r := chi.NewRouter()
//	r.Use(middleware.Logger)        // <--<< Logger should come before Recoverer
//	r.Use(middleware.Recoverer)
//	r.Get("/", handler)
func Logger(next http.Handler) http.Handler {
	return DefaultLogger(next)
}

// RequestLogger returns a logger handler using a custom LogFormatter.
func RequestLogger(f LogFormatter) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			entry := f.NewLogEntry(r)
			ww := NewWrapResponseWriter(w, r.ProtoMajor)

			t1 := time.Now()
			defer func() {
				entry.Write(ww.Status(), ww.BytesWritten(), ww.Header(), time.Since(t1), nil)
			}()

			next.ServeHTTP(ww, WithLogEntry(r, entry))
		}
		return http.HandlerFunc(fn)
	}
}

// SettlementExtra is passed as the extra argument to LogEntry.Write by
// SettlingRequestLogger. It carries the single settlement reason of the
// request. LogEntry implementations that do not know about it simply
// ignore extra, so existing formatters keep working and keep their exact
// output.
type SettlementExtra struct {
	Reason SettlementReason
}

// SettlingRequestLogger is RequestLogger with per-request settlement turned
// on. It behaves like RequestLogger, except that:
//
//   - exactly one settlement record is written per request, even when the
//     logger is mounted on several routers along one chain (for example a
//     root router and mounted subrouters): only the outermost settling
//     logger creates a [Settlement] and writes a record, inner layers share
//     it and stay silent;
//   - the Settlement is shared through the request context and its reason
//     is first-wins, so a later layer can never overwrite an earlier one;
//   - the reason is passed to LogEntry.Write as a [SettlementExtra] in the
//     extra argument. [DefaultLogFormatter] renders it only when
//     DefaultLogFormatter.WithReason is set, otherwise the output is
//     byte-identical to RequestLogger's.
//
// Client disconnects and reached deadlines are observable before the
// handler returns via [GetSettlement]; panics are marked by [Recoverer]
// before recovery runs, and unmatched routes settle as [ReasonUnmatched].
func SettlingRequestLogger(f LogFormatter) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			// An outer settling layer (typically on a parent router) already
			// owns this request's single settlement record: share it and do
			// not write a second record for this request.
			if GetSettlement(r.Context()) != nil {
				next.ServeHTTP(w, r)
				return
			}

			s := acquireSettlement()
			r = WithSettlement(r, s)

			entry := f.NewLogEntry(r)
			ww := NewWrapResponseWriter(w, r.ProtoMajor)

			t1 := time.Now()
			stop := s.watch(r.Context())

			defer func() {
				stop()
				if rvr := recover(); rvr != nil {
					// No Recoverer handled this: record the outcome in the
					// single settlement record first, then keep the panic
					// propagating so existing behavior is unchanged.
					if rvr == http.ErrAbortHandler {
						s.Mark(ReasonAborted)
					} else {
						s.Mark(ReasonPanic)
					}
					entry.Write(ww.Status(), ww.BytesWritten(), ww.Header(), time.Since(t1), SettlementExtra{Reason: s.Reason()})
					releaseSettlement(s)
					panic(rvr)
				}

				finalizeSettlement(s, r)
				entry.Write(ww.Status(), ww.BytesWritten(), ww.Header(), time.Since(t1), SettlementExtra{Reason: s.Reason()})
				releaseSettlement(s)
			}()

			next.ServeHTTP(ww, WithLogEntry(r, entry))
		}
		return http.HandlerFunc(fn)
	}
}

// LogFormatter initiates the beginning of a new LogEntry per request.
// See DefaultLogFormatter for an example implementation.
type LogFormatter interface {
	NewLogEntry(r *http.Request) LogEntry
}

// LogEntry records the final log when a request completes.
// See defaultLogEntry for an example implementation.
type LogEntry interface {
	Write(status, bytes int, header http.Header, elapsed time.Duration, extra any)
	Panic(v any, stack []byte)
}

// GetLogEntry returns the in-context LogEntry for a request.
func GetLogEntry(r *http.Request) LogEntry {
	entry, _ := r.Context().Value(LogEntryCtxKey).(LogEntry)
	return entry
}

// WithLogEntry sets the in-context LogEntry for a request.
func WithLogEntry(r *http.Request, entry LogEntry) *http.Request {
	r = r.WithContext(context.WithValue(r.Context(), LogEntryCtxKey, entry))
	return r
}

// LoggerInterface accepts printing to stdlib logger or compatible logger.
type LoggerInterface interface {
	Print(v ...any)
}

// DefaultLogFormatter is a simple logger that implements a LogFormatter.
type DefaultLogFormatter struct {
	Logger  LoggerInterface
	NoColor bool

	// WithReason appends the settlement reason (" reason=<reason>") to the
	// record when the request was handled by SettlingRequestLogger and a
	// reason could be determined. It is opt-in: when false, and for the
	// plain RequestLogger, the output is unchanged. The status, byte count
	// and elapsed fields are never altered.
	WithReason bool
}

// NewLogEntry creates a new LogEntry for the request.
func (l *DefaultLogFormatter) NewLogEntry(r *http.Request) LogEntry {
	ctx := r.Context()

	useColor := !l.NoColor
	entry := &defaultLogEntry{
		DefaultLogFormatter: l,
		request:             r,
		buf:                 &bytes.Buffer{},
		useColor:            useColor,
	}

	reqID := GetReqID(ctx)
	if reqID != "" {
		cW(entry.buf, useColor, nYellow, "[%s] ", reqID)
	}
	cW(entry.buf, useColor, nCyan, "\"")
	cW(entry.buf, useColor, bMagenta, "%s ", r.Method)

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	cW(entry.buf, useColor, nCyan, "%s://%s%s %s\" ", scheme, r.Host, r.RequestURI, r.Proto)

	entry.buf.WriteString("from ")
	clientIP := GetClientIP(ctx)
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}
	entry.buf.WriteString(clientIP)
	entry.buf.WriteString(" - ")

	return entry
}

type defaultLogEntry struct {
	*DefaultLogFormatter
	request  *http.Request
	buf      *bytes.Buffer
	useColor bool
}

func (l *defaultLogEntry) Write(status, bytes int, header http.Header, elapsed time.Duration, extra any) {
	switch {
	case status < 200:
		cW(l.buf, l.useColor, bBlue, "%03d", status)
	case status < 300:
		cW(l.buf, l.useColor, bGreen, "%03d", status)
	case status < 400:
		cW(l.buf, l.useColor, bCyan, "%03d", status)
	case status < 500:
		cW(l.buf, l.useColor, bYellow, "%03d", status)
	default:
		cW(l.buf, l.useColor, bRed, "%03d", status)
	}

	cW(l.buf, l.useColor, bBlue, " %dB", bytes)

	l.buf.WriteString(" in ")
	if elapsed < 500*time.Millisecond {
		cW(l.buf, l.useColor, nGreen, "%s", elapsed)
	} else if elapsed < 5*time.Second {
		cW(l.buf, l.useColor, nYellow, "%s", elapsed)
	} else {
		cW(l.buf, l.useColor, nRed, "%s", elapsed)
	}

	// Append the settlement reason only when explicitly enabled and only
	// when a reason is available; everything above stays byte-identical.
	if l.WithReason {
		if se, ok := extra.(SettlementExtra); ok && se.Reason != "" {
			l.buf.WriteString(" reason=")
			l.buf.WriteString(string(se.Reason))
		}
	}

	l.Logger.Print(l.buf.String())
}

func (l *defaultLogEntry) Panic(v any, stack []byte) {
	printPrettyStack(v, l.useColor)
}

func init() {
	color := true
	if runtime.GOOS == "windows" {
		color = false
	}
	DefaultLogger = RequestLogger(&DefaultLogFormatter{Logger: log.New(os.Stdout, "", log.LstdFlags), NoColor: !color})
}
