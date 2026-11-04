// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gateway

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type gatewayClientIPContextKey struct{}

func gatewayClientIPMiddleware(next http.Handler, trustedCIDRs []string) http.Handler {
	prefixes := make([]netip.Prefix, 0, len(trustedCIDRs))
	for _, value := range trustedCIDRs {
		prefix, err := netip.ParsePrefix(value)
		if err == nil {
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peer, ok := gatewayRemoteIP(request.RemoteAddr)
		if !ok {
			next.ServeHTTP(writer, request)
			return
		}
		client := peer
		if gatewayIPInPrefixes(peer, prefixes) {
			values := strings.Split(strings.Join(request.Header.Values("X-Forwarded-For"), ","), ",")
			addresses := make([]netip.Addr, 0, len(values))
			valid := len(values) > 0
			for _, value := range values {
				address, err := netip.ParseAddr(strings.TrimSpace(value))
				if err != nil {
					valid = false
					break
				}
				addresses = append(addresses, address.Unmap())
			}
			if valid && len(addresses) > 0 {
				client = addresses[0]
				for index := len(addresses) - 1; index >= 0; index-- {
					if !gatewayIPInPrefixes(addresses[index], prefixes) {
						client = addresses[index]
						break
					}
				}
			}
		}
		request = request.WithContext(context.WithValue(request.Context(), gatewayClientIPContextKey{}, client.String()))
		next.ServeHTTP(writer, request)
	})
}

func gatewayRemoteIP(remote string) (netip.Addr, bool) {
	if addressPort, err := netip.ParseAddrPort(remote); err == nil {
		return addressPort.Addr().Unmap(), true
	}
	if host, _, err := net.SplitHostPort(remote); err == nil {
		if address, err := netip.ParseAddr(host); err == nil {
			return address.Unmap(), true
		}
	}
	address, err := netip.ParseAddr(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

func gatewayIPInPrefixes(address netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
