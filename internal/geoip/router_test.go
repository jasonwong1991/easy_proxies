package geoip

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRequestUsesRegionHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/jp/path", nil)
	req.Header.Set(regionHeader, "US")

	region, target, err := (&Router{}).parseRequest(req)
	if err != nil {
		t.Fatalf("parseRequest() error = %v", err)
	}
	if region != RegionUS {
		t.Fatalf("region = %q, want %q", region, RegionUS)
	}
	if target != "example.com" {
		t.Fatalf("target = %q, want %q", target, "example.com")
	}
	if got := req.URL.Path; got != "/jp/path" {
		t.Fatalf("path = %q, want header selection to preserve it", got)
	}
	if got := req.Header.Get(regionHeader); got != "" {
		t.Fatalf("region header was not removed: %q", got)
	}
}

func TestParseRequestRejectsInvalidRegionHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
	req.Header.Set(regionHeader, "invalid")

	_, _, err := (&Router{}).parseRequest(req)
	if err == nil {
		t.Fatal("parseRequest() error = nil, want invalid region rejection")
	}
	if got := req.Header.Get(regionHeader); got != "" {
		t.Fatalf("region header was not removed: %q", got)
	}
}

func TestParseRequestFallsBackToRegionalConnectAuthority(t *testing.T) {
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(
		"CONNECT jp/example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
	)))
	if err != nil {
		t.Fatalf("ReadRequest() error = %v", err)
	}

	region, target, err := (&Router{}).parseRequest(req)
	if err != nil {
		t.Fatalf("parseRequest() error = %v", err)
	}
	if region != RegionJP {
		t.Fatalf("region = %q, want %q", region, RegionJP)
	}
	if target != "example.com:443" {
		t.Fatalf("target = %q, want %q", target, "example.com:443")
	}
}
