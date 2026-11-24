// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// trustedProxyList holds CIDR ranges of reverse proxies allowed to supply
// X-Forwarded-For / X-Real-IP / X-Forwarded-Proto headers. When empty or nil,
// proxy forwarding headers are ignored for client IP resolution so untrusted
// callers cannot spoof their address to bypass rate limits.
type trustedProxyList struct {
	prefixes []netip.Prefix
}

func parseTrustedProxies(raw string) (*trustedProxyList, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';'
	})
	prefixes := make([]netip.Prefix, 0, len(parts))
	for _, entry := range parts {
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", entry, err)
			}
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy IP %q: %w", entry, err)
		}
		addr = addr.Unmap()
		prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	return &trustedProxyList{prefixes: prefixes}, nil
}

func (t *trustedProxyList) contains(addr netip.Addr) bool {
	if t == nil || len(t.prefixes) == 0 || !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range t.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (t *trustedProxyList) isTrustedPeer(r *http.Request) bool {
	peer, ok := parseAddrString(remoteHost(r))
	return ok && t.contains(peer)
}

func parseAddrString(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// clientIP resolves the effective client IP for rate limiting and device audit.
// When the direct TCP peer is in the trusted proxy list, X-Forwarded-For is
// walked right-to-left skipping trusted hops; otherwise r.RemoteAddr is returned.
func (t *trustedProxyList) clientIP(r *http.Request) string {
	direct := remoteHost(r)
	peer, ok := parseAddrString(direct)
	if !ok || !t.contains(peer) {
		return direct
	}
	if xffValues := r.Header.Values("X-Forwarded-For"); len(xffValues) > 0 {
		hops := strings.Split(strings.Join(xffValues, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			addr, valid := parseAddrString(hops[i])
			if !valid {
				return peer.String()
			}
			if !t.contains(addr) {
				return addr.String()
			}
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		if addr, valid := parseAddrString(xri); valid && !t.contains(addr) {
			return addr.String()
		}
	}
	return peer.String()
}

// isSecure reports whether the client connection used HTTPS. When trusted
// proxies are configured, X-Forwarded-Proto is only honored from a trusted peer.
func (t *trustedProxyList) isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if t != nil && len(t.prefixes) > 0 && !t.isTrustedPeer(r) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
