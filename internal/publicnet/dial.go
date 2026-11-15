package publicnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

var ErrNonPublicDestination = errors.New("non-public destination blocked")

// Resolver and Dialer are seams for DNS and socket tests. Production uses the
// host resolver and a net.Dialer; the container never receives either one.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

var blockedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
}

func IsPublicIP(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.Is4In6() || addr.Zone() != "" {
		return false
	}
	if addr.Is4() {
		for _, prefix := range blockedIPv4 {
			if prefix.Contains(addr) {
				return false
			}
		}
		return true
	}
	return netip.MustParsePrefix("2000::/3").Contains(addr) && !netip.MustParsePrefix("2001:db8::/32").Contains(addr)
}

// ResolvePublic rejects the entire DNS answer if any address is non-public.
// The caller must connect to a returned literal IP rather than resolving the
// hostname again, otherwise DNS rebinding can bypass this check.
func ResolvePublic(ctx context.Context, host string, resolver Resolver) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		if !IsPublicIP(literal) {
			return nil, ErrNonPublicDestination
		}
		return []netip.Addr{literal}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("host resolved to no addresses")
	}
	for _, addr := range addresses {
		if !IsPublicIP(addr) {
			return nil, fmt.Errorf("host resolved to a non-public address: %w", ErrNonPublicDestination)
		}
	}
	return addresses, nil
}

// DialHTTP pins an approved public address for a web TCP connection. HTTP(S)
// egress is limited to ordinary ports; it cannot be used as a generic tunnel
// to arbitrary public services or deployment control ports.
func DialHTTP(ctx context.Context, host, port string, resolver Resolver, dialer Dialer) (net.Conn, error) {
	if port != "80" && port != "443" {
		return nil, errors.New("nonstandard public HTTP port blocked")
	}
	addresses, err := ResolvePublic(ctx, host, resolver)
	if err != nil {
		return nil, err
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	var dialErr error
	for _, address := range addresses {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErr = errors.Join(dialErr, err)
		if ctx.Err() != nil {
			return nil, errors.Join(dialErr, ctx.Err())
		}
	}
	return nil, dialErr
}
