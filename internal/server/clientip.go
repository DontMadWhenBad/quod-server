package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ProxyTrust decides which peers may tell us the real client address.
// Forwarding headers are honoured only when the direct peer is a trusted
// proxy (e.g. a local cloudflared); otherwise anyone could spoof their IP.
type ProxyTrust struct {
	nets []netip.Prefix
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or addresses
// ("127.0.0.1/32,::1", empty = trust nobody).
func ParseTrustedProxies(s string) (ProxyTrust, error) {
	var t ProxyTrust
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return ProxyTrust{}, fmt.Errorf("trusted proxy %q: %w", f, err)
			}
			f = netip.PrefixFrom(a, a.BitLen()).String()
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return ProxyTrust{}, fmt.Errorf("trusted proxy %q: %w", f, err)
		}
		t.nets = append(t.nets, p.Masked())
	}
	return t, nil
}

func (t ProxyTrust) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range t.nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the address of the real client:
//   - the direct peer, unless it is a trusted proxy;
//   - otherwise CF-Connecting-IP if present;
//   - otherwise the right-most X-Forwarded-For entry that is not itself a
//     trusted proxy (entries to its left are client-controlled).
func (t ProxyTrust) ClientIP(r *http.Request) netip.Addr {
	peer := parseHostAddr(r.RemoteAddr)
	if !peer.IsValid() || !t.trusted(peer) {
		return peer
	}
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if a, err := netip.ParseAddr(cf); err == nil {
			return a.Unmap()
		}
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // malformed: stop at the last address we could trust
		}
		a = a.Unmap()
		if !t.trusted(a) {
			return a
		}
	}
	return peer
}

func parseHostAddr(hostport string) netip.Addr {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}
