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
	lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "inside.example" {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	t.Cleanup(func() { lookupIP = net.DefaultResolver.LookupIPAddr })
	req := func(u string) *http.Request {
		p, _ := url.Parse(u)
		return (&http.Request{URL: p}).WithContext(context.Background())
	}
	from := []*http.Request{req("https://feed.example/rss")}
	for target, ok := range map[string]bool{
		"https://cdn.example/rss":         true,
		"https://inside.example/rss":      false, // resolves to a private address
		"http://169.254.169.254/latest/":  false, // downgrade and link-local
		"https://169.254.169.254/latest/": false, // link-local (cloud metadata)
		"https://127.0.0.1:8080/":         false,
		"https://[::1]/":                  false,
		"https://100.101.102.103/":        false, // CGNAT / Tailscale
		"https://100.63.255.255/":         true,  // just below 100.64/10
		"https://100.128.0.1/":            true,  // just above 100.64/10
		"http://cdn.example/rss":          false, // https -> http
	} {
		if err := CheckRedirect(req(target), from); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", target, err, ok)
		}
	}
	if err := CheckRedirect(req("http://cdn.example/"), []*http.Request{req("http://feed.example/")}); err != nil {
		t.Errorf("http -> http to a public host must be allowed: %v", err)
	}
	many := make([]*http.Request, maxRedirects)
	for i := range many {
		many[i] = from[0]
	}
	if err := CheckRedirect(req("https://cdn.example/"), many); err == nil {
		t.Error("want error after too many redirects")
	}
}

func TestGetRefusesRedirectToLoopback(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer target.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/internal", http.StatusFound)
	}))
	defer feed.Close()
	c := &http.Client{CheckRedirect: CheckRedirect}
	_, err := get(context.Background(), c, feed.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("want refused redirect, got %v", err)
	}
}
