package webutils

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

type staticResolver struct {
	records map[string][]netip.Addr
	err     error
}

func (r staticResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.records[host], nil
}

func TestValidateAndResolveURL(t *testing.T) {
	public := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("10.0.0.1")
	cases := []struct {
		name    string
		rawURL  string
		records map[string][]netip.Addr
		errIs   error
	}{
		{
			name:   "accepts public https destination",
			rawURL: "https://example.com",
			records: map[string][]netip.Addr{
				"example.com": {public},
			},
		},
		{
			name:   "rejects non-https scheme",
			rawURL: "http://example.com",
			errIs:  errURLMustBeHTTPS,
		},
		{
			name:   "rejects private literal ip",
			rawURL: "https://10.0.0.1",
			errIs:  errHostNotPublic,
		},
		{
			name:   "rejects mixed dns answers",
			rawURL: "https://mixed.example",
			records: map[string][]netip.Addr{
				"mixed.example": {public, private},
			},
			errIs: errHostNotPublic,
		},
		{
			name:   "rejects host without answers",
			rawURL: "https://missing.example",
			errIs:  errHostNoPublicAddress,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateAndResolveURL(context.Background(), staticResolver{records: tc.records}, tc.rawURL)
			if tc.errIs == nil && err != nil {
				t.Fatalf("validateAndResolveURL returned error: %v", err)
			}
			if tc.errIs != nil && !errors.Is(err, tc.errIs) {
				t.Fatalf("expected %v, got %v", tc.errIs, err)
			}
		})
	}
}

func TestIsPublicAddress(t *testing.T) {
	if !isPublicAddress(netip.MustParseAddr("93.184.216.34")) {
		t.Fatal("expected public IPv4 to be accepted")
	}
	if isPublicAddress(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("expected loopback IPv4 to be rejected")
	}
	if isPublicAddress(netip.MustParseAddr("fc00::1")) {
		t.Fatal("expected IPv6 ULA to be rejected")
	}
}
