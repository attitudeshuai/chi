package chi

import "slices"

// RouteDiagReason explains why a single branch that the router tried while
// matching a request did not produce a handler.
type RouteDiagReason string

const (
	// RouteDiagPrefixMismatch means a static segment was the next candidate,
	// but its literal prefix did not match the remaining request path.
	// 路径前缀不符。
	RouteDiagPrefixMismatch RouteDiagReason = "prefix_mismatch"

	// RouteDiagParamEmpty means a parameter or regexp segment required a
	// non-empty value, but the remaining request path was empty.
	// 参数为空。
	RouteDiagParamEmpty RouteDiagReason = "param_empty"

	// RouteDiagParamMismatch is a finer-grained variant of a parameter segment
	// failure: the segment had a value but it did not fit the parameter shape
	// (its closing delimiter was absent, or it would cross a '/' boundary).
	// 参数段形状不符。
	RouteDiagParamMismatch RouteDiagReason = "param_mismatch"

	// RouteDiagRegexpMismatch means a regexp segment was reached with a value,
	// but the value did not match the segment's regular expression.
	// 正则不匹配。
	RouteDiagRegexpMismatch RouteDiagReason = "regexp_mismatch"

	// RouteDiagMethodNotAllowed means the path matched a route leaf, but the
	// leaf has no handler for the request's HTTP method. The methods actually
	// registered on that leaf are listed in RouteDiagAttempt.AllowedMethods.
	// 方法不允许。
	RouteDiagMethodNotAllowed RouteDiagReason = "method_not_allowed"

	// RouteDiagWildcardUncovered means routing entered a catch-all branch
	// (including a mounted subrouter's wildcard) but the remainder of the path
	// was not covered by any handler underneath it.
	// 通配未覆盖。
	RouteDiagWildcardUncovered RouteDiagReason = "wildcard_uncovered"

	// RouteDiagNoRoute is a finer-grained reason: a parameter or regexp
	// segment consumed a value, but no route existed for the remainder of the
	// path beneath it.
	// 参数段命中后，后续路径无路由。
	RouteDiagNoRoute RouteDiagReason = "no_route"
)

// RouteDiagNodeType identifies the kind of routing node a branch attempt was
// made against.
type RouteDiagNodeType string

const (
	// RouteDiagStatic is a literal path segment, e.g. /users.
	RouteDiagStatic RouteDiagNodeType = "static"
	// RouteDiagParam is a {param} segment.
	RouteDiagParam RouteDiagNodeType = "param"
	// RouteDiagRegexp is a {param:regexp} segment.
	RouteDiagRegexp RouteDiagNodeType = "regexp"
	// RouteDiagCatchAll is a trailing * segment.
	RouteDiagCatchAll RouteDiagNodeType = "catchall"
)

// RouteDiagAttempt is one branch that the router tried while matching a
// request, in the order it was tried. The Level field is the mount depth of
// the attempt (0 for the root router, 1 for a directly mounted subrouter,
// and so on), so attempts from nested subrouters stay attributable.
type RouteDiagAttempt struct {
	// Level is the mount depth the attempt belongs to (root = 0).
	Level int

	// NodeType is the kind of branch node that was tried.
	NodeType RouteDiagNodeType

	// Reason is why this attempt did not produce a handler.
	Reason RouteDiagReason

	// Pattern is the registered routing pattern associated with the node,
	// when one is known (e.g. "/users/{id}" on a method-not-allowed leaf).
	Pattern string

	// Segment is the literal static prefix or the regexp source of the node
	// that was tried, when one is available.
	Segment string

	// AllowedMethods is populated only for method_not_allowed attempts; it
	// lists, in a stable order, the HTTP methods registered on that leaf.
	AllowedMethods []string
}

// RouteDiagnostics is the report of a single failed route match.
//
// The report is never written to the response and never influences routing;
// it is a read-only, per-request snapshot that can be retrieved from the
// routing Context after routing, e.g. from a custom NotFound handler.
type RouteDiagnostics struct {
	// Attempts is every branch tried during the match, in attempt order
	// across the whole stack of mounted subrouters.
	Attempts []RouteDiagAttempt

	// MethodNotAllowed separates the two kinds of misses: true means the
	// path matched a route but the method was unsupported (HTTP 405); false
	// means no route matched the path (HTTP 404).
	MethodNotAllowed bool

	// AllowedMethods lists, in a stable order, the methods actually allowed
	// on the matched path when MethodNotAllowed is true; nil otherwise.
	AllowedMethods []string
}

// routeDiag is the per-request collector attached to a Context. It is
// allocated only when diagnostics are explicitly enabled, so that disabled
// routing performs no collection work.
//
// inlineDiagAttempts covers the attempts of ordinary requests, including a
// couple of mount levels. Records stay allocation-free up to this size, so
// an enabled request that ends up matching pays no collection cost beyond
// the collector itself; only larger misses grow the slice.
const inlineDiagAttempts = 4

type routeDiag struct {
	attempts []routeDiagAttempt

	// head is the inline backing for attempts, keeping the common case
	// allocation-free after the single collector allocation.
	head [inlineDiagAttempts]routeDiagAttempt

	// depth is the number of mux levels currently routing; the active
	// level index is depth-1 between an enterLevel/leaveLevel pair
	// (root router = 0).
	depth int

	// decided/matched/mna are the outcome of the deepest routing level,
	// which is the outcome of the whole request.
	decided bool
	matched bool
	mna     bool

	allowed []methodTyp
}

type routeDiagAttempt struct {
	level    int
	nodeType RouteDiagNodeType
	reason   RouteDiagReason
	pattern  string
	segment  string
	allowed  []methodTyp
}

// newRouteDiag creates a collector with an inline attempt buffer.
func newRouteDiag() *routeDiag {
	d := &routeDiag{}
	d.attempts = d.head[:0]
	return d
}

// level returns the mount depth currently routing (root router = 0).
func (d *routeDiag) level() int {
	return d.depth - 1
}

// enterLevel marks the start of routing on one mux level. Calls nest
// across mounted subrouters.
func (d *routeDiag) enterLevel() {
	d.depth++
}

// leaveLevel closes the routing level started by enterLevel.
func (d *routeDiag) leaveLevel() {
	d.depth--
}

// markMatched records that the deepest routing level produced a handler.
func (d *routeDiag) markMatched() {
	d.decided = true
	d.matched = true
}

// markMiss records that the deepest routing level produced no handler.
func (d *routeDiag) markMiss(methodNotAllowed bool, allowed []methodTyp) {
	d.decided = true
	d.matched = false
	d.mna = methodNotAllowed
	d.allowed = append(d.allowed[:0], allowed...)
}

// record appends one failed branch attempt at the given mount level.
func (d *routeDiag) record(n *node, level int, ntyp nodeTyp, reason RouteDiagReason) {
	a := routeDiagAttempt{
		level:    level,
		nodeType: nodeTypeName(ntyp),
		reason:   reason,
	}
	if n != nil {
		a.segment = n.prefix
		a.pattern = nodePattern(n)
		if reason == RouteDiagMethodNotAllowed {
			a.allowed = nodeAllowedMethods(n)
		}
	}
	d.attempts = append(d.attempts, a)
}

// snapshot returns a stable, detached copy of the report. It returns nil
// unless diagnostics were enabled, routing reached a decision, and the
// request ultimately did not match.
func (d *routeDiag) snapshot() *RouteDiagnostics {
	if d == nil || !d.decided || d.matched {
		return nil
	}

	snap := &RouteDiagnostics{
		MethodNotAllowed: d.mna,
		AllowedMethods:   methodTypeNames(d.allowed),
		Attempts:         make([]RouteDiagAttempt, 0, len(d.attempts)),
	}
	for _, a := range d.attempts {
		snap.Attempts = append(snap.Attempts, RouteDiagAttempt{
			Level:          a.level,
			NodeType:       a.nodeType,
			Reason:         a.reason,
			Pattern:        a.pattern,
			Segment:        a.segment,
			AllowedMethods: methodTypeNames(a.allowed),
		})
	}
	return snap
}

func nodeTypeName(ntyp nodeTyp) RouteDiagNodeType {
	switch ntyp {
	case ntStatic:
		return RouteDiagStatic
	case ntParam:
		return RouteDiagParam
	case ntRegexp:
		return RouteDiagRegexp
	case ntCatchAll:
		return RouteDiagCatchAll
	default:
		return ""
	}
}

// nodePattern returns a registered pattern associated with a node,
// preferring the catch-all/mount entry so the result is deterministic
// regardless of map iteration order.
func nodePattern(n *node) string {
	if n.endpoints == nil {
		return ""
	}
	if e := n.endpoints[mALL]; e != nil && e.pattern != "" {
		return e.pattern
	}
	if e := n.endpoints[mSTUB]; e != nil && e.pattern != "" {
		return e.pattern
	}
	for _, e := range n.endpoints {
		if e != nil && e.pattern != "" {
			return e.pattern
		}
	}
	return ""
}

// nodeAllowedMethods returns the concrete HTTP methods with a handler on the
// node, sorted by method bit so repeated requests yield identical output.
func nodeAllowedMethods(n *node) []methodTyp {
	if n.endpoints == nil {
		return nil
	}
	var out []methodTyp
	for mt, e := range n.endpoints {
		if mt == mALL || mt == mSTUB || e == nil || e.handler == nil {
			continue
		}
		out = append(out, mt)
	}
	slices.Sort(out)
	return out
}

func methodTypeNames(ms []methodTyp) []string {
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, mt := range ms {
		if name, ok := reverseMethodMap[mt]; ok {
			out = append(out, name)
		}
	}
	return out
}
