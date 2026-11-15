package publicnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestDialHTTPPinsPublicResolutionAndRejectsMixedDNS(t *testing.T) {
	ctx := context.Background()
	dials := 0
	dialer := dialerFunc(func(_ context.Context, network, address string) (net.Conn, error) {
		dials++
		if network != "tcp" || address != "8.8.8.8:443" {
			t.Fatalf("dial was not pinned to approved IP: %q %q", network, address)
		}
		left, right := net.Pipe()
		t.Cleanup(func() { _ = right.Close() })
		return left, nil
	})
	resolver := resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "public.example" {
			t.Fatalf("wrong lookup: %q %q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	conn, err := DialHTTP(ctx, "public.example", "443", resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if dials != 1 {
		t.Fatalf("public dials = %d", dials)
	}
	for _, addresses := range [][]netip.Addr{
		{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")},
		{netip.MustParseAddr("10.0.0.2")},
	} {
		resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return addresses, nil })
		if _, err := DialHTTP(ctx, "public.example", "443", resolver, dialer); err == nil {
			t.Fatalf("unsafe DNS answer was accepted: %v", addresses)
		}
	}
	if _, err := DialHTTP(ctx, "169.254.169.254", "80", resolver, dialer); err == nil {
		t.Fatal("metadata address was accepted")
	}
	if _, err := DialHTTP(ctx, "8.8.8.8", "8080", resolver, dialer); err == nil {
		t.Fatal("nonstandard port was accepted")
	}
	if dials != 1 {
		t.Fatalf("unsafe targets reached dialer: %d", dials)
	}
}

func TestResolvePublicPropagatesResolverFailure(t *testing.T) {
	want := errors.New("DNS unavailable")
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, want })
	if _, err := ResolvePublic(context.Background(), "public.example", resolver); !errors.Is(err, want) {
		t.Fatalf("resolver error = %v", err)
	}
}

func TestDialHTTPTriesOnlyValidatedPublicAnswers(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
	})
	var dialed []string
	dialer := dialerFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if address == "8.8.8.8:443" {
			return nil, errors.New("first public address unavailable")
		}
		left, right := net.Pipe()
		t.Cleanup(func() { _ = right.Close() })
		return left, nil
	})
	conn, err := DialHTTP(context.Background(), "public.example", "443", resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if len(dialed) != 2 || dialed[0] != "8.8.8.8:443" || dialed[1] != "1.1.1.1:443" {
		t.Fatalf("public fallback attempts = %v", dialed)
	}

	resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}, nil
	})
	if _, err := DialHTTP(context.Background(), "mixed.example", "443", resolver, dialer); err == nil {
		t.Fatal("mixed public/private DNS answer reached fallback dialer")
	}
	if len(dialed) != 2 {
		t.Fatalf("rejected mixed answer reached dialer: %v", dialed)
	}
}
