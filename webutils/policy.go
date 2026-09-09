package webutils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

const (
	maxURLLength      = 2048
	maxHostnameLength = 253
)

var (
	errURLRequired           = errors.New("url is required")
	errURLTooLong            = errors.New("url exceeds maximum length")
	errURLMustBeHTTPS        = errors.New("url must use https")
	errURLHostRequired       = errors.New("url host is required")
	errURLUserinfoNotAllowed = errors.New("url userinfo is not allowed")
	errHostNotPublic         = errors.New("url host resolves to a non-public destination")
	errHostNoPublicAddress   = errors.New("url host has no usable public address")
)

var disallowedIPPrefixes = mustPrefixes(
	"0.0.0.0/8",       // "this" network, including unspecified.
	"10.0.0.0/8",      // RFC1918.
	"100.64.0.0/10",   // CGNAT.
	"127.0.0.0/8",     // loopback.
	"169.254.0.0/16",  // link-local.
	"172.16.0.0/12",   // RFC1918.
	"192.0.0.0/24",    // IETF protocol assignments.
	"192.0.2.0/24",    // TEST-NET-1.
	"192.88.99.0/24",  // 6to4 relay anycast (deprecated).
	"192.168.0.0/16",  // RFC1918.
	"198.18.0.0/15",   // benchmarking.
	"198.51.100.0/24", // TEST-NET-2.
	"203.0.113.0/24",  // TEST-NET-3.
	"224.0.0.0/4",     // multicast.
	"240.0.0.0/4",     // reserved/future use.
	"::/128",          // unspecified.
	"::1/128",         // loopback.
	"64:ff9b:1::/48",  // local-use IPv4/IPv6 translation.
	"100::/64",        // discard-only block.
	"2001::/32",       // TEREDO.
	"2001:2::/48",     // benchmarking.
	"2001:db8::/32",   // documentation.
	"fc00::/7",        // ULA.
	"fe80::/10",       // link-local unicast.
	"ff00::/8",        // multicast.
)

func mustPrefixes(raw ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			panic(err)
		}
		out = append(out, prefix)
	}
	return out
}

type policyResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

func validateAndResolveURL(ctx context.Context, resolver policyResolver, raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errURLRequired
	}
	if len(raw) > maxURLLength {
		return nil, errURLTooLong
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, errURLMustBeHTTPS
	}
	if parsed.Host == "" {
		return nil, errURLHostRequired
	}
	if parsed.User != nil {
		return nil, errURLUserinfoNotAllowed
	}
	host := parsed.Hostname()
	if host == "" || len(host) > maxHostnameLength {
		return nil, errURLHostRequired
	}
	if err := ensurePublicHost(ctx, resolver, host); err != nil {
		return nil, err
	}
	return parsed, nil
}

func ensurePublicHost(ctx context.Context, resolver policyResolver, host string) error {
	if parsed, err := netip.ParseAddr(host); err == nil {
		if !isPublicAddress(parsed) {
			return errHostNotPublic
		}
		return nil
	}
	ips, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("dns resolution failed: %w", err)
	}
	if len(ips) == 0 {
		return errHostNoPublicAddress
	}
	allowed := 0
	for _, ip := range ips {
		if !isPublicAddress(ip) {
			return errHostNotPublic
		}
		allowed++
	}
	if allowed == 0 {
		return errHostNoPublicAddress
	}
	return nil
}

func isPublicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range disallowedIPPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func defaultResolver() policyResolver {
	return net.DefaultResolver
}
