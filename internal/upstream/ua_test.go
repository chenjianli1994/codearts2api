package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// B45：出站标识。配置了 User-Agent 就补到没有显式 UA 的请求上；
// 请求自己带了 UA 时不覆盖；清空配置后不再补。
func TestOutboundUserAgentOverride(t *testing.T) {
	var seen string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Get("User-Agent")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})
	tr := uaTransport{base: base}

	SetUserAgent("CodeArts-Manager/1.2")
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if seen != "CodeArts-Manager/1.2" {
		t.Fatalf("应补上配置的 UA，got=%q", seen)
	}

	req2, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	req2.Header.Set("User-Agent", "keep-me")
	if _, err := tr.RoundTrip(req2); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if seen != "keep-me" {
		t.Fatalf("显式 UA 不应被覆盖，got=%q", seen)
	}

	SetUserAgent("")
	req3, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	if _, err := tr.RoundTrip(req3); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if seen != "" {
		t.Fatalf("清空配置后不应补 UA，got=%q", seen)
	}
}
