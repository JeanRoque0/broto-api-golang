package api

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"
)

func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	if raw == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		p, e := netip.ParsePrefix(strings.TrimSpace(part))
		if e != nil || p.Bits() == 0 {
			return nil, errors.New("invalid TRUSTED_PROXY_CIDRS; never trust every address")
		}
		out = append(out, p)
	}
	return out, nil
}
func (s *Server) clientIP(r *http.Request) string {
	remote := remoteIP(r)
	trusted := func(addr netip.Addr) bool {
		for _, p := range s.trustedProxies {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	addr, e := netip.ParseAddr(remote)
	if e != nil || !trusted(addr) {
		return remote
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) > 20 {
		return remote
	}
	for i := len(chain) - 1; i >= 0; i-- {
		addr, e = netip.ParseAddr(strings.TrimSpace(chain[i]))
		if e != nil {
			return remote
		}
		if !trusted(addr) {
			return addr.String()
		}
	}
	return remote
}
