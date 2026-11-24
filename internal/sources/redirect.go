package sources

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

const maxRedirects = 10

// CheckRedirect is an http.Client redirect policy: it refuses to downgrade
// https to http and stops after maxRedirects. Which addresses may be reached
// is enforced at connect time by Transport. Errors never include the URL
// (Telegram's holds the bot token).
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing redirect from https to " + req.URL.Scheme)
	}
	return nil
}

// Transport returns an HTTP transport that refuses to connect to loopback,
// private, CGNAT, link-local, multicast or unspecified addresses, so a
// compromised feed can't point newsagent at internal services. The check runs
// on the address actually dialed, after DNS resolution, so DNS rebinding
// can't slip past it. Hosts in allow (the configured feeds) may be private.
func Transport(allow []string) *http.Transport {
	ok := make(map[string]bool, len(allow))
	for _, h := range allow {
		ok[strings.ToLower(h)] = true
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		d := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		if !ok[strings.ToLower(host)] {
			d.Control = func(_, address string, _ syscall.RawConn) error {
				ip, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				if p := net.ParseIP(ip); p == nil || !public(p) {
					return fmt.Errorf("refusing to connect to %s: non-public address %s", host, ip)
				}
				return nil
			}
		}
		return d.DialContext(ctx, network, addr)
	}
	return t
}

// cgnat is the shared address space (RFC 6598); Tailscale uses it for IPv4.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func public(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip))
}
