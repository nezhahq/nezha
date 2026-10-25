package rpc

import (
	"context"
	"github.com/nezhahq/nezha/pkg/utils"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Channel names of the panel's own TokenLookup (controller/jwt.go
// "header: Authorization, query: token, cookie: nz-jwt"). A dashboard JWT
// can therefore reach the NAT ingress through the Authorization header, the
// ?token= query parameter or the nz-jwt cookie, and all three must be
// classified before the request is tunneled to the agent.
const (
	natDashboardJWTCookieName     = "nz-jwt"
	natDashboardJWTQueryParameter = "token"
	// Keep credential classification work constant even when an unauthenticated
	// NAT request contains thousands of token parameters or cookies. Values
	// beyond this per-request budget fail closed and are stripped.
	maxNATDashboardCredentialValues = 16
)

var (
	natDashboardCredentialGateMu sync.RWMutex
	// natDashboardCredentialValueGate classifies a bounded batch of raw
	// credential values. Batching lets the controller resolve all PAT hashes in
	// one query instead of turning attacker-controlled query/cookie fan-out into
	// one database query per value.
	natDashboardCredentialValueGate func(ctx context.Context, values []string) []bool
)

// SetNATDashboardCredentialGate wires the dashboard-credential classifier
// used by the NAT ingress. main() injects
// controller.ClassifyDashboardCredentialValues;
// the setter keeps the rpc package free of a dependency on the controller
// package, the same pattern as SetReceiptGateListener and
// SetMCPKillSwitchObserver. A nil function fails closed: every value on that
// channel is then treated as dashboard-issued and stripped.
func SetNATDashboardCredentialGate(valueClass func(ctx context.Context, values []string) []bool) {
	natDashboardCredentialGateMu.Lock()
	natDashboardCredentialValueGate = valueClass
	natDashboardCredentialGateMu.Unlock()
}

// NATDashboardCredentialGateWired reports whether the dashboard-credential
// classifier has been wired. It exists so the main() wiring contract can be
// pinned by a test.
func NATDashboardCredentialGateWired() bool {
	natDashboardCredentialGateMu.RLock()
	wired := natDashboardCredentialValueGate != nil
	natDashboardCredentialGateMu.RUnlock()
	return wired
}

// stripDashboardCredentials removes credentials issued by this dashboard from
// every channel the panel's own TokenLookup accepts: the Authorization
// header, the ?token= query parameter and the nz-jwt cookie. Such credentials
// authenticate the dashboard itself and must never become origin credentials
// for the NAT backend. Foreign values are forwarded untouched: the NAT
// backend may authenticate with credentials of its own, and this dashboard
// must not make policy for them.
func stripDashboardCredentials(request *http.Request) {
	classification := classifyNATDashboardCredentialValues(request)
	stripDashboardAuthorizationHeader(request, classification)
	stripDashboardCredentialQuery(request, classification)
	stripDashboardCredentialCookie(request, classification)
}

type natDashboardCredentialClassification struct {
	wired   bool
	limited bool
	issued  map[string]bool
}

func (c natDashboardCredentialClassification) isDashboardCredential(value string) bool {
	if !c.wired {
		return true
	}
	if issued, ok := c.issued[value]; ok {
		return issued
	}
	// Only candidate values collected from this request reach this method. A
	// missing result therefore means the distinct-value budget was exceeded;
	// fail closed so a dashboard credential cannot be hidden after junk values.
	return c.limited
}

func classifyNATDashboardCredentialValues(request *http.Request) natDashboardCredentialClassification {
	natDashboardCredentialGateMu.RLock()
	gate := natDashboardCredentialValueGate
	natDashboardCredentialGateMu.RUnlock()
	classification := natDashboardCredentialClassification{
		wired:  gate != nil,
		issued: make(map[string]bool),
	}
	if gate == nil {
		return classification
	}

	values, limited := collectNATDashboardCredentialValues(request)
	classification.limited = limited
	results := gate(request.Context(), values)
	if len(results) != len(values) {
		// A broken classifier must not turn into credential forwarding.
		for _, value := range values {
			classification.issued[value] = true
		}
		classification.limited = true
		return classification
	}
	for i, value := range values {
		classification.issued[value] = results[i]
	}
	return classification
}

func collectNATDashboardCredentialValues(request *http.Request) ([]string, bool) {
	values := make([]string, 0, maxNATDashboardCredentialValues)
	seen := make(map[string]struct{}, maxNATDashboardCredentialValues)
	limited := false
	add := func(value string) {
		if _, ok := seen[value]; ok {
			return
		}
		if len(values) == maxNATDashboardCredentialValues {
			limited = true
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}

	for _, authorization := range request.Header.Values("Authorization") {
		credential := utils.BearerCredential(authorization)
		if credential != "" {
			add(credential)
		}
	}
	if request.URL != nil {
		for _, element := range strings.Split(request.URL.RawQuery, "&") {
			if value, ok := dashboardCredentialQueryElementValue(element); ok {
				add(value)
			}
		}
	}
	for _, line := range request.Header.Values("Cookie") {
		for _, pair := range strings.Split(line, ";") {
			if value, ok := dashboardCredentialCookiePairValue(pair); ok {
				add(value)
			}
		}
	}
	return values, limited
}

// stripDashboardAuthorizationHeader removes the Authorization header when it
// carries a dashboard-issued credential.
func stripDashboardAuthorizationHeader(request *http.Request, classification natDashboardCredentialClassification) {
	if !classification.wired {
		// Gate not wired (early startup or tests): fail closed and keep the
		// historical behavior of stripping every Authorization header.
		request.Header.Del("Authorization")
		return
	}
	// A multi-valued Authorization header is malformed traffic, but any
	// dashboard-issued value in it must still not survive into the tunnel, so
	// the whole header is dropped when one is recognized.
	for _, value := range request.Header.Values("Authorization") {
		// Use the same scheme parser as MCP OAuth authentication; lowercase
		// bearer must not become a credential-forwarding bypass.
		credential := utils.BearerCredential(value)
		if credential != "" && classification.isDashboardCredential(credential) {
			request.Header.Del("Authorization")
			return
		}
	}
}

// stripDashboardCredentialQuery removes ?token= query parameters whose value
// is a dashboard-issued credential. Every other element of the query string —
// including foreign token values — is preserved byte-for-byte, so the
// forwarded request keeps the exact query the client sent.
func stripDashboardCredentialQuery(request *http.Request, classification natDashboardCredentialClassification) {
	if request.URL == nil || request.URL.RawQuery == "" {
		return
	}
	elements := strings.Split(request.URL.RawQuery, "&")
	kept := elements[:0]
	changed := false
	for _, element := range elements {
		if !classification.wired && dashboardCredentialQueryElementName(element) {
			changed = true
			continue
		}
		value, candidate := dashboardCredentialQueryElementValue(element)
		if candidate && classification.isDashboardCredential(value) {
			changed = true
			continue
		}
		kept = append(kept, element)
	}
	if changed {
		request.URL.RawQuery = strings.Join(kept, "&")
	}
}

// dashboardCredentialQueryElementValue extracts the decoded value from a raw
// query element carrying the panel's token parameter.
func dashboardCredentialQueryElementValue(element string) (string, bool) {
	rawName, rawValue, _ := strings.Cut(element, "=")
	// The panel reads the parameter by its decoded name (gin-jwt c.Query),
	// so an escaped key must be decoded before comparing.
	name, err := url.QueryUnescape(rawName)
	if err != nil || name != natDashboardJWTQueryParameter {
		return "", false
	}
	// A value that fails to decode is one the panel's own query parsing drops
	// as well, so it can never be a panel credential.
	decoded, err := url.QueryUnescape(rawValue)
	if err != nil {
		return "", false
	}
	return decoded, true
}

func dashboardCredentialQueryElementName(element string) bool {
	rawName, _, _ := strings.Cut(element, "=")
	name, err := url.QueryUnescape(rawName)
	return err == nil && name == natDashboardJWTQueryParameter
}

// stripDashboardCredentialCookie removes nz-jwt cookies whose value is a
// dashboard-issued credential. All other cookie pairs — including foreign
// nz-jwt values — are preserved verbatim.
func stripDashboardCredentialCookie(request *http.Request, classification natDashboardCredentialClassification) {
	lines := request.Header.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	changed := false
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		nextLine, lineChanged := stripDashboardCredentialCookieLine(line, classification)
		changed = changed || lineChanged
		if nextLine != "" {
			kept = append(kept, nextLine)
		}
	}
	if !changed {
		return
	}
	request.Header.Del("Cookie")
	for _, line := range kept {
		request.Header.Add("Cookie", line)
	}
}

// stripDashboardCredentialCookieLine rewrites a single Cookie header line,
// dropping the dashboard-issued nz-jwt pairs. Kept pairs keep their raw form,
// whitespace included — except that a pair following a dropped one no longer
// carries the removed pair's share of the "; " separator.
func stripDashboardCredentialCookieLine(line string, classification natDashboardCredentialClassification) (string, bool) {
	pairs := strings.Split(line, ";")
	kept := pairs[:0]
	droppedBeforeFirstKept := true
	changed := false
	for _, pair := range pairs {
		if !classification.wired && dashboardCredentialCookiePairName(pair) {
			changed = true
			continue
		}
		value, candidate := dashboardCredentialCookiePairValue(pair)
		if candidate && classification.isDashboardCredential(value) {
			changed = true
			continue
		}
		if droppedBeforeFirstKept {
			pair = strings.TrimLeft(pair, " ")
		}
		droppedBeforeFirstKept = false
		kept = append(kept, pair)
	}
	if !changed {
		return line, false
	}
	return strings.Join(kept, ";"), true
}

// dashboardCredentialCookiePairValue extracts a cookie value the same way the
// panel hands it to the JWT verifier: surrounding whitespace/quotes are
// presentation, then Gin applies QueryUnescape.
func dashboardCredentialCookiePairValue(pair string) (string, bool) {
	name, rawValue, found := strings.Cut(pair, "=")
	if !found || strings.TrimSpace(name) != natDashboardJWTCookieName {
		return "", false
	}
	value := strings.Trim(strings.TrimSpace(rawValue), `"`)
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return "", false
	}
	return decoded, true
}

func dashboardCredentialCookiePairName(pair string) bool {
	name, _, found := strings.Cut(pair, "=")
	return found && strings.TrimSpace(name) == natDashboardJWTCookieName
}
