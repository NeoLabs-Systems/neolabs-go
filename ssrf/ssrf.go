// Package ssrf gives outbound HTTP clients protection against server-side
// request forgery: every destination IP is checked after DNS resolution and
// the checked address is the one that gets dialled, so DNS rebinding cannot
// swap in an internal address. Redirects are re-validated hop by hop.
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// ErrBlocked is returned for destinations that are not public internet hosts.
var ErrBlocked = errors.New("ssrf: destination is not allowed")

var extraBlocked = mustPrefixes(
	"0.0.0.0/8",       // "this" network
	"100.64.0.0/10",   // carrier-grade NAT
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved + broadcast
	"64:ff9b::/96",    // NAT64 can embed internal IPv4 addresses
	"2001:db8::/32",   // documentation
)

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, s := range list {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// CheckAddr rejects loopback, private, link-local (including the cloud
// metadata address 169.254.169.254), multicast, unspecified and reserved addresses.
func CheckAddr(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("%w: %s", ErrBlocked, addr)
	}
	for _, p := range extraBlocked {
		if p.Contains(addr) {
			return fmt.Errorf("%w: %s", ErrBlocked, addr)
		}
	}
	return nil
}

// CheckURL accepts only http(s) URLs with a host and no embedded credentials.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid url", ErrBlocked)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q", ErrBlocked, u.Scheme)
	}
	if u.Hostname() == "" || u.User != nil {
		return nil, fmt.Errorf("%w: host or credentials", ErrBlocked)
	}
	return u, nil
}

// Options tune NewClient. AllowPrivate exists for tests and for deployments
// that deliberately call internal services; it is off by default.
type Options struct {
	AllowPrivate bool
	MaxRedirects int           // default 5
	Timeout      time.Duration // whole request, default 30s
}

// NewClient returns an HTTP client that refuses internal destinations. It
// ignores proxy environment variables so the dial-time check cannot be bypassed.
func NewClient(o Options) *http.Client {
	if o.MaxRedirects == 0 {
		o.MaxRedirects = 5
	}
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Second
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:               nil,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			var lastErr error = fmt.Errorf("%w: no addresses for %s", ErrBlocked, host)
			for _, ip := range ips {
				if !o.AllowPrivate {
					if err := CheckAddr(ip); err != nil {
						lastErr = err
						continue
					}
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
	}
	return &http.Client{
		Timeout:   o.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > o.MaxRedirects {
				return fmt.Errorf("ssrf: stopped after %d redirects", o.MaxRedirects)
			}
			_, err := CheckURL(req.URL.String())
			return err
		},
	}
}
