package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

// Transport decides what a request's connection can be trusted with: who the client is (for
// slowing down guessing), and whether passwords and session cookies may cross it.
type Transport struct {
	// TrustedProxies are reverse proxies whose X-Forwarded-For and X-Forwarded-Proto are believed
	// (TENSORAFI_TRUSTED_PROXIES). Anyone else's are ignored.
	TrustedProxies []netip.Prefix
	// AllowPlainHTTP allows sign-in and session cookies over plain HTTP from other machines
	// (TENSORAFI_INSECURE_HTTP=true), for a network the operator trusts.
	AllowPlainHTTP bool
}

// forwardingHeaders are what reverse proxies add. From a peer that isn't a trusted proxy they mean
// a proxy the operator hasn't listed, or a client making them up. Either way they can't be
// believed, and the peer's address may stand for everyone behind that proxy.
var forwardingHeaders = []string{
	"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Server", "Forwarded", "Via",
	"X-Real-Ip", "X-Client-Ip", "Cf-Connecting-Ip", "True-Client-Ip",
}

func forwarded(r *http.Request) bool {
	for _, h := range forwardingHeaders {
		if len(r.Header.Values(h)) > 0 {
			return true
		}
	}
	return false
}

func (t Transport) peer(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	return a.Unmap(), err == nil
}

func (t Transport) trusted(a netip.Addr) bool {
	for _, p := range t.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// viaTrustedProxy reports whether r came from a trusted proxy.
func (t Transport) viaTrustedProxy(r *http.Request) bool {
	a, ok := t.peer(r)
	return ok && t.trusted(a)
}

// parseHop reads one X-Forwarded-For entry: an address, with or without a port.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if a, err := netip.ParseAddr(host); err == nil {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// Client is the caller's address, for slowing down guessing, or false if it can't be told. From a
// trusted proxy, it's the nearest X-Forwarded-For entry (across every header line) that isn't a
// trusted proxy, since each proxy appends the address it saw; entries further left can be made up
// by the client. If every entry is a trusted proxy, it's the leftmost one. An entry that can't be
// read means the client can't be told: guessing must not share one limit with everyone else.
func (t Transport) Client(r *http.Request) (netip.Addr, bool) {
	peer, ok := t.peer(r)
	if !ok || !t.trusted(peer) {
		return peer, ok
	}
	lines := r.Header.Values("X-Forwarded-For")
	if len(lines) == 0 {
		if forwarded(r) {
			// Proxied, but without saying who for: everyone behind it would share one limit.
			return netip.Addr{}, false
		}
		return peer, true // the proxy's own request, or this machine's browser beside it
	}
	hops := strings.Split(strings.Join(lines, ","), ",")
	var a netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		h, ok := parseHop(hops[i])
		if !ok {
			return netip.Addr{}, false
		}
		if a = h; !t.trusted(h) {
			break
		}
	}
	return a, true
}

// forwardedProto is what the nearest proxy says the client used: the last X-Forwarded-Proto value.
func forwardedProto(r *http.Request) string {
	lines := r.Header.Values("X-Forwarded-Proto")
	if len(lines) == 0 {
		return ""
	}
	values := strings.Split(lines[len(lines)-1], ",")
	return strings.ToLower(strings.TrimSpace(values[len(values)-1]))
}

// Secure reports whether r arrived over HTTPS: directly, or through a trusted proxy that says so.
func (t Transport) Secure(r *http.Request) bool {
	return r.TLS != nil || (t.viaTrustedProxy(r) && forwardedProto(r) == "https")
}

var (
	errPlainHTTP = domain.Errorf(domain.CodeForbidden,
		"signing in needs HTTPS or this machine; on a network you trust, the operator can set TENSORAFI_INSECURE_HTTP=true")
	errUnlistedProxy = domain.Errorf(domain.CodeForbidden,
		"this request came through a proxy that isn't in TENSORAFI_TRUSTED_PROXIES; the operator needs to list it")
	errUnknownClient = domain.Errorf(domain.CodeForbidden,
		"the proxy's X-Forwarded-For doesn't say who you are; the operator needs to fix the proxy")
)

// CheckPrivate says why passwords and session cookies may not cross r's connection, or returns
// nil when they may: over HTTPS, from a browser on this machine (a loopback peer with no proxy
// headers), or with the operator's opt-in. A request with proxy headers from a peer that isn't a
// trusted proxy is refused even then: whatever is in front can't be checked, and everyone behind
// it would share one limit on guessing. A proxy that adds no headers at all can't be told from a
// browser, which is why proxies must be listed and must send X-Forwarded-For and -Proto.
func (t Transport) CheckPrivate(r *http.Request) error {
	peer, ok := t.peer(r)
	switch {
	case forwarded(r) && (!ok || !t.trusted(peer)):
		return errUnlistedProxy
	case t.Secure(r) || t.AllowPlainHTTP:
		return nil
	case ok && peer.IsLoopback() && !forwarded(r):
		return nil
	}
	return errPlainHTTP
}

// Private reports whether passwords and session cookies may cross r's connection (CheckPrivate).
func (t Transport) Private(r *http.Request) bool { return t.CheckPrivate(r) == nil }

// SessionCookieName is the cookie's name for r: over HTTPS, the __Host- form, which a browser only
// accepts with Secure, Path=/, and no Domain, so another site or subdomain can't plant or overwrite it.
func (t Transport) SessionCookieName(r *http.Request) string {
	if t.Secure(r) {
		return SecureSessionCookie
	}
	return SessionCookie
}
