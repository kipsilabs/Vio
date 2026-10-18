package remotestream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

const (
	maxRedirects          = 5
	responseHeaderTimeout = 30 * time.Second
)

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// NewSafeTransport returns a transport that resolves and validates every new
// connection, then dials the validated IP directly. This closes the DNS
// rebinding window between validation and connection establishment.
func NewSafeTransport() *http.Transport {
	return newSafeTransport(net.DefaultResolver, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second})
}

func newSafeTransport(resolver ipResolver, dialer contextDialer) *http.Transport {
	return buildTransport(resolver, dialer, true)
}

// NewInsecureTransport returns a transport that pins the resolved IP of every
// new connection without rejecting private, loopback, link-local, or reserved
// addresses. It exists only so the allow_private_streams admin opt-in can
// actually reach private-host stream URLs; every other path must use
// NewSafeTransport.
func NewInsecureTransport() *http.Transport {
	return buildTransport(net.DefaultResolver, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, false)
}

func newInsecureTransport(resolver ipResolver, dialer contextDialer) *http.Transport {
	return buildTransport(resolver, dialer, false)
}

func buildTransport(resolver ipResolver, dialer contextDialer, enforcePublic bool) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	transport.Proxy = nil
	// Bound connection stalls without imposing a deadline on the response
	// body. Media bodies may legitimately stream for hours.
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse remote stream address: %w", err)
		}
		var addresses []netip.Addr
		var resolveErr error
		if enforcePublic {
			addresses, resolveErr = resolvePublicAddresses(ctx, resolver, host)
		} else {
			addresses, resolveErr = resolveAnyAddresses(ctx, resolver, host)
		}
		if resolveErr != nil {
			return nil, resolveErr
		}
		var dialErr error
		for _, candidate := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = errors.Join(dialErr, err)
		}
		return nil, fmt.Errorf("dial validated remote stream host: %w", dialErr)
	}
	transport.DialTLSContext = nil
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConnsPerHost = 10
	transport.MaxConnsPerHost = 32
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // secure minimum; ServerName is populated by net/http.
	return transport
}

// resolveAnyAddresses resolves a host to its addresses without applying the
// public-address SSRF filter. For literal numeric hosts the parsed address is
// returned directly. Only callers that have explicitly opted into insecure
// (private/local) remote URLs should use this.
func resolveAnyAddresses(ctx context.Context, resolver ipResolver, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, errors.New("resolve remote stream host: lookup failed")
	}
	if len(addresses) == 0 {
		return nil, errors.New("remote stream host has no address")
	}
	result := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.Unmap())
	}
	return result, nil
}

// NewSafeClient returns an HTTP client that applies the safe transport and
// validates every redirect target. HTTPS-to-HTTP redirects are rejected.
func NewSafeClient() *http.Client {
	return &http.Client{
		Transport:     NewSafeTransport(),
		CheckRedirect: checkRedirect,
	}
}

func NewInsecureClient() *http.Client {
	return &http.Client{
		Transport:     NewInsecureTransport(),
		CheckRedirect: checkRedirectAllowNonPublic,
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	return checkRedirectWithValidator(req, via, func(ctx context.Context, raw string) error {
		_, err := validateURL(ctx, raw, net.DefaultResolver)
		return err
	})
}

func checkRedirectAllowNonPublic(req *http.Request, via []*http.Request) error {
	return checkRedirectWithValidator(req, via, func(_ context.Context, raw string) error {
		_, err := ValidateURLSyntaxAllowNonPublic(raw)
		return err
	})
}

func checkRedirectWithValidator(req *http.Request, via []*http.Request, validator func(context.Context, string) error) error {
	if len(via) >= maxRedirects {
		return errors.New("too many remote stream redirects")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("remote stream redirect would downgrade HTTPS")
	}
	if err := validator(req.Context(), req.URL.String()); err != nil {
		return fmt.Errorf("unsafe remote stream redirect: %w", err)
	}
	return nil
}
