package sources

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCheckRedirect(t *testing.T) {
	req := func(u string) *http.Request {
		p, _ := url.Parse(u)
		return &http.Request{URL: p}
	}
	from := []*http.Request{req("https://feed.example/rss")}
	if err := CheckRedirect(req("https://cdn.example/rss"), from); err != nil {
		t.Errorf("https -> https: %v", err)
	}
	if err := CheckRedirect(req("http://cdn.example/rss"), from); err == nil {
		t.Error("https -> http must be refused")
	}
	if err := CheckRedirect(req("http://cdn.example/"), []*http.Request{req("http://feed.example/")}); err != nil {
		t.Errorf("http -> http must be allowed: %v", err)
	}
	many := make([]*http.Request, maxRedirects)
	for i := range many {
		many[i] = from[0]
	}
	if err := CheckRedirect(req("https://cdn.example/"), many); err == nil {
		t.Error("want error after too many redirects")
	}
}

func TestPublic(t *testing.T) {
	for ip, want := range map[string]bool{
		"93.184.216.34":   true,
		"10.0.0.5":        false,
		"192.168.1.1":     false,
		"169.254.169.254": false, // link-local (cloud metadata)
		"127.0.0.1":       false,
		"::1":             false,
		"fd7a:115c::1":    false, // Tailscale IPv6 (ULA)
		"100.101.102.103": false, // CGNAT / Tailscale IPv4
		"100.63.255.255":  true,  // just below 100.64/10
		"100.128.0.1":     true,  // just above 100.64/10
		"0.0.0.0":         false,
		"224.0.0.1":       false,
	} {
		if got := public(net.ParseIP(ip)); got != want {
			t.Errorf("public(%s) = %v, want %v", ip, got, want)
		}
	}
}

// loopbackServer serves body on 127.0.0.1 and returns its port.
func loopbackServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, string) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return srv, port
}

func TestTransportChecksDialedAddress(t *testing.T) {
	_, port := loopbackServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("INTERNAL")) })
	feed, _ := loopbackServer(t, func(w http.ResponseWriter, r *http.Request) {
		// "localhost" is not on the allow list; it resolves to loopback only
		// at connect time, like a rebinding DNS answer would.
		http.Redirect(w, r, "http://localhost:"+port+"/latest", http.StatusFound)
	})
	c := &http.Client{Transport: Transport([]string{"127.0.0.1"}), CheckRedirect: CheckRedirect}
	if _, err := get(context.Background(), c, feed.URL, nil); err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("redirect to a host resolving to loopback: want refused, got %v", err)
	}
	// The allowed (configured) host itself still works, also when it is private.
	if body, err := get(context.Background(), c, "http://127.0.0.1:"+port+"/", nil); err != nil || string(body) != "INTERNAL" {
		t.Fatalf("allowed private host: %q %v", body, err)
	}
	// A host that is not allowed is refused even without a redirect.
	c = &http.Client{Transport: Transport(nil)}
	if _, err := get(context.Background(), c, "http://127.0.0.1:"+port+"/", nil); err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("not-allowed private host: want refused, got %v", err)
	}
}
