package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"go-harness/internal/publicnet"
	"go-harness/web"
)

type fetchDialFunc func(context.Context, string, string) (net.Conn, error)

func (f fetchDialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type fetchResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f fetchResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestPublicIPPolicy(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "198.18.0.1", "::1", "fc00::1", "fe80::1", "::ffff:8.8.8.8", "2001:db8::1"} {
		if publicnet.IsPublicIP(netip.MustParseAddr(address)) {
			t.Errorf("accepted non-public address %s", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicnet.IsPublicIP(netip.MustParseAddr(address)) {
			t.Errorf("rejected public address %s", address)
		}
	}
}

func TestURLPolicy(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "http://user:pass@example.com/", "http://example.com./", "https://example.com/" + strings.Repeat("x", maxWebURLChars)} {
		_, err := validateURL(raw)
		if err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if _, err := validateURL("https://example.com:8443/"); err != nil {
		t.Fatalf("public HTTP(S) URL with an explicit port was rejected: %v", err)
	}
	if _, err := (Fetcher{}).Fetch(context.Background(), "http://127.0.0.1:8080/"); err == nil {
		t.Fatal("private destination on explicit port was accepted")
	}
	if _, err := (Fetcher{}).Fetch(context.Background(), "http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("metadata destination was accepted")
	}
}

func TestFetchFollowsFiveSameOriginRedirectsAndRejectsSixth(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		count, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil {
			t.Error(err)
			return
		}
		if count < 5 {
			http.Redirect(w, r, fmt.Sprintf("/?n=%d", count+1), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "final")
	})
	result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/?n=0")
	if err != nil || result.Body.Content != "final" || result.URL != "http://8.8.8.8/?n=5" {
		t.Fatalf("five redirects: %+v, %v", result, err)
	}
	limitFetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		count, _ := strconv.Atoi(r.URL.Query().Get("n"))
		http.Redirect(w, r, fmt.Sprintf("/?n=%d", count+1), http.StatusFound)
	})
	if _, err := limitFetcher.Fetch(context.Background(), "http://8.8.8.8/?n=0"); err == nil || !strings.Contains(err.Error(), "maximum of 5 redirects") {
		t.Fatalf("sixth redirect = %v", err)
	}
}

func TestFetchRejectsRedirectWithoutLocation(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	})
	if _, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/missing"); err == nil || !strings.Contains(err.Error(), "Location") {
		t.Fatalf("redirect without Location = %v", err)
	}
}

func testFetcher(t *testing.T, handler http.HandlerFunc) Fetcher {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return Fetcher{Dialer: fetchDialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})}
}

func TestFetchPinsApprovedAddressAndReturnsDecodedHTML(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "8.8.8.8" {
			t.Errorf("unexpected host %s", r.Host)
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Hidden title</title></head><body><h1>Visible heading</h1><script>secret</script><p hidden>hidden</p><p>Useful body</p></body></html>`)
	})
	original := fetcher.Dialer
	fetcher.Dialer = fetchDialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "8.8.8.8:80" {
			t.Errorf("unvalidated dial target %s", address)
		}
		return original.DialContext(ctx, network, address)
	})
	result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/start")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "http://8.8.8.8/final" || result.StatusCode != 200 || result.Body.Kind != "html" || !strings.Contains(result.Body.Content, "<h1>Visible heading</h1>") || !strings.Contains(result.Body.Content, "Useful body") {
		t.Fatalf("bad extracted page: %#v", result)
	}
}

func TestFetchFallsBackOnlyAmongValidatedPublicAddresses(t *testing.T) {
	var dialed []string
	fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "fallback succeeded")
	})
	fetcher.Resolver = fetchResolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "public.example" {
			t.Fatalf("unexpected lookup %q %q", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
	})
	actualDial := fetcher.Dialer
	fetcher.Dialer = fetchDialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		if address == "8.8.8.8:80" {
			return nil, errors.New("first public address unavailable")
		}
		return actualDial.DialContext(ctx, network, address)
	})
	result, err := fetcher.Fetch(context.Background(), "http://public.example/")
	if err != nil || result.Body.Content != "fallback succeeded" || len(dialed) != 2 || dialed[0] != "8.8.8.8:80" || dialed[1] != "1.1.1.1:80" {
		t.Fatalf("public fallback = %+v, dials=%v, error=%v", result, dialed, err)
	}
	fetcher.Resolver = fetchResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, nil
	})
	dialed = nil
	if _, err := fetcher.Fetch(context.Background(), "http://public.example/"); err == nil || len(dialed) != 0 {
		t.Fatalf("mixed DNS answer reached dialer: dials=%v, error=%v", dialed, err)
	}
}

func TestFetchAcceptsUpstreamStructuredTextTypesButRejectsBinary(t *testing.T) {
	for _, contentType := range []string{"text/css", "text/markdown", "application/xml", "application/atom+xml", "application/problem+json"} {
		t.Run(contentType, func(t *testing.T) {
			fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				fmt.Fprint(w, "text body")
			})
			result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/structured")
			if err != nil || result.Body.Content != "text body" || result.Truncated {
				t.Fatalf("content type %s: %+v, %v", contentType, result, err)
			}
		})
	}
	fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, "binary body")
	})
	if _, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/binary"); err == nil {
		t.Fatal("binary content type was accepted")
	}
}

func TestFetchRejectsCrossOriginRedirectBeforeDial(t *testing.T) {
	dials := 0
	fetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://9.9.9.9/private", http.StatusFound)
	})
	original := fetcher.Dialer
	fetcher.Dialer = fetchDialFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		return original.DialContext(ctx, network, address)
	})
	if _, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/start"); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("cross-origin redirect result = %v", err)
	}
	if dials != 1 {
		t.Fatalf("redirect dial count = %d, want 1", dials)
	}
}

func TestFetchRejectsPrivateDestinationBeforeDial(t *testing.T) {
	dials := 0
	fetcher := Fetcher{Dialer: fetchDialFunc(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, nil
	})}
	if _, err := fetcher.Fetch(context.Background(), "http://127.0.0.1/"); err == nil {
		t.Fatal("private destination was accepted")
	}
	if dials != 0 {
		t.Fatal("private destination reached dialer")
	}
}

func TestFetchCapsBody(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", maxResponseBytes+1))
	})
	result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/")
	if err != nil || !result.Truncated || len(result.Body.Content) != maxBodyChars {
		t.Fatalf("bounded fetch = %d chars, truncated=%t, error=%v", len(result.Body.Content), result.Truncated, err)
	}
}

func TestFetchAcceptsDeclaredBodyAboveFormerTwoMiBLimit(t *testing.T) {
	const bodyBytes = 2<<20 + 1
	fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(bodyBytes))
		fmt.Fprint(w, strings.Repeat("x", bodyBytes))
	})
	result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/large")
	if err != nil || !result.Truncated || len(result.Body.Content) != maxBodyChars {
		t.Fatalf("declared body above 2 MiB = %d chars, truncated=%t, error=%v", len(result.Body.Content), result.Truncated, err)
	}
}

func TestFetchCapsDecodedBodyByUTF16BeforeHTMLRendering(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<p>"+strings.Repeat("🧪", maxBodyChars/2)+"</p><p>hidden tail</p>")
	})
	result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/html")
	if err != nil || !result.Truncated || strings.Contains(result.Body.Content, "hidden tail") || !strings.Contains(result.Body.Content, "🧪") {
		t.Fatalf("UTF-16 HTML source cap = %d bytes, truncated=%t, error=%v", len(result.Body.Content), result.Truncated, err)
	}
}

func TestFetchCapsTextByUTF16WithoutSplittingEmoji(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, strings.Repeat("🧪", maxBodyChars/2+1))
	})
	result, err := fetcher.Fetch(context.Background(), "http://8.8.8.8/text")
	if err != nil || !result.Truncated || len([]rune(result.Body.Content)) != maxBodyChars/2 || !strings.HasSuffix(result.Body.Content, "🧪") {
		t.Fatalf("UTF-16 text cap = %d runes, truncated=%t, error=%v", len([]rune(result.Body.Content)), result.Truncated, err)
	}
}

func TestFetchDecodesUpstreamCharsetsAndBOM(t *testing.T) {
	for _, fixture := range []struct{ name, contentType, body, expected string }{
		{"UTF-8 BOM", "text/plain", "\xef\xbb\xbfRésumé", "Résumé"},
		{"Latin-1 alias", "text/plain; charset=ISO-8859-1", "\x80 caf\xe9", "€ café"},
		{"UTF-16LE BOM", "text/plain; charset=utf-16", "\xff\xfeA\x00", "A"},
		{"UTF-16BE BOM", "text/plain; charset=utf-16be", "\xfe\xff\x00A", "A"},
		{"joined UTF-8 charset", "text/plain; charset=utf-8, custom", "Résumé", "Résumé"},
		{"malformed charset with invalid bytes", "text/plain; charset=utf-8, custom", "\xff", ""},
		{"unknown charset", "text/plain; charset=x-unsupported", "body", ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			fetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() != "deepseek-harness/0.0.1 (+https://github.com/deepseek-ai)" {
					t.Errorf("unexpected fetch User-Agent %q", r.UserAgent())
				}
				w.Header().Set("Content-Type", fixture.contentType)
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, fixture.body)
			})
			got, err := fetcher.Fetch(t.Context(), "HTTP://8.8.8.8:80/a/../page")
			if fixture.expected == "" {
				var failure *web.Error
				if !errors.As(err, &failure) || failure.Code != "WEB_UNSUPPORTED_CONTENT_TYPE" {
					t.Fatalf("invalid charset was not rejected: %v", err)
				}
				return
			}
			if err != nil || got.URL != "http://8.8.8.8/page" || got.StatusCode != 403 ||
				got.Body.Kind != "text" || got.Body.Content != fixture.expected {
				t.Fatalf("decoded fetch = %+v, %v", got, err)
			}
		})
	}
}

func TestFetchRejectsDeclaredOversizeBeforeReading(t *testing.T) {
	fetcher := testFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(maxResponseBytes+1))
		w.WriteHeader(http.StatusOK)
	})
	_, err := fetcher.Fetch(t.Context(), "http://8.8.8.8/")
	var failure *web.Error
	if !errors.As(err, &failure) || failure.Code != "WEB_FETCH_TOO_LARGE" {
		t.Fatalf("oversize fetch: %v", err)
	}
}
