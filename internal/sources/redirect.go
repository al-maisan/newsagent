package sources

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

const maxRedirects = 10

// lookupIP resolves redirect targets (a var for tests).
var lookupIP = net.DefaultResolver.LookupIPAddr

// CheckRedirect is an http.Client redirect policy: it refuses to downgrade
// https to http and to follow a redirect to a loopback, private, link-local
// or unspecified address, so a compromised feed can't point newsagent at
// internal services. Errors never include the URL (Telegram's holds the bot
// token).
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing redirect from https to " + req.URL.Scheme)
	}
	host := req.URL.Hostname()
	ips, err := resolve(req.Context(), host)
	if err != nil {
		return fmt.Errorf("redirect to %s: %w", host, err)
	}
	for _, ip := range ips {
		if !public(ip) {
			return fmt.Errorf("refusing redirect to %s: non-public address %s", host, ip)
		}
	}
	return nil
}

func resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := lookupIP(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// cgnat is the shared address space (RFC 6598); Tailscale uses it for IPv4.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func public(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip))
}
