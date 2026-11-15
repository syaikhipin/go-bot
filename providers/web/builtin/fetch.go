package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	whatwg "github.com/nlnwa/whatwg-url/url"
	"go-harness/internal/publicnet"
	"go-harness/web"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
)

const (
	maxResponseBytes       = 5_000_000
	maxBodyChars           = 100_000
	maxWebURLChars         = 2048
	maxSameOriginRedirects = 5
)

// Fetcher retrieves and decodes an anonymous public HTTP(S) response.
// Address resolution is checked and pinned before every connection.
type Fetcher struct {
	Resolver publicnet.Resolver
	Dialer   publicnet.Dialer
}

func (f Fetcher) Fetch(ctx context.Context, rawURL string) (web.FetchResult, error) {
	if ctx.Err() != nil {
		return web.FetchResult{}, fetchFailure(ctx, ctx.Err())
	}
	current, err := validateURL(rawURL)
	if err != nil {
		return web.FetchResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport := &http.Transport{
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
		DialContext:            f.dialPublic,
	}
	defer transport.CloseIdleConnections()
	for redirects := 0; ; redirects++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.Href(true), nil)
		if err != nil {
			return web.FetchResult{}, webError("WEB_INVALID_URL", "invalid URL: "+current.Href(false), err)
		}
		req.Header.Set("Accept", "text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8")
		req.Header.Set("User-Agent", "deepseek-harness/0.0.1 (+https://github.com/deepseek-ai)")
		resp, err := transport.RoundTrip(req)
		if err != nil {
			return web.FetchResult{}, fetchFailure(ctx, err)
		}
		if !isRedirect(resp.StatusCode) {
			defer resp.Body.Close()
			return readBody(ctx, resp, current.Href(false))
		}
		resp.Body.Close()
		if redirects >= maxSameOriginRedirects {
			return web.FetchResult{}, webError("WEB_REDIRECT_BLOCKED", "exceeded the maximum of 5 redirects", nil)
		}
		location := resp.Header.Values("Location")
		if len(location) == 0 {
			return web.FetchResult{}, webError("WEB_PROVIDER_ERROR", fmt.Sprintf("redirect response (HTTP %d) without a Location header", resp.StatusCode), nil)
		}
		target, err := current.Parse(strings.Join(location, ", "))
		if err != nil {
			return web.FetchResult{}, webError("WEB_PROVIDER_ERROR", fmt.Sprintf("invalid redirect Location %q", strings.Join(location, ", ")), err)
		}
		target, err = validateURL(target.Href(false))
		if err != nil {
			return web.FetchResult{}, err
		}
		if current.Scheme() != target.Scheme() || current.Host() != target.Host() {
			return web.FetchResult{}, webError("WEB_REDIRECT_BLOCKED", "cross-origin redirect to "+target.Scheme()+"://"+target.Host()+" is not followed automatically; retry against that URL directly", nil)
		}
		current = target
	}
}

func (f Fetcher) dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := publicnet.ResolvePublic(ctx, host, f.Resolver)
	if err != nil {
		if errors.Is(err, publicnet.ErrNonPublicDestination) {
			return nil, webError("WEB_BLOCKED_URL", err.Error(), err)
		}
		return nil, err
	}
	dial := f.Dialer
	if dial == nil {
		dial = &net.Dialer{}
	}
	var dialErr error
	for _, ip := range ips {
		connection, err := dial.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return connection, nil
		}
		dialErr = errors.Join(dialErr, err)
		if ctx.Err() != nil {
			return nil, errors.Join(dialErr, ctx.Err())
		}
	}
	return nil, dialErr
}

func validateURL(raw string) (*whatwg.Url, error) {
	if utf16Length(raw) > maxWebURLChars {
		return nil, webError("WEB_INVALID_URL", "URL exceeds the maximum length of 2048", nil)
	}
	u, err := whatwg.Parse(raw)
	if err != nil {
		return nil, webError("WEB_INVALID_URL", "invalid URL: "+raw, err)
	}
	if u.Scheme() != "http" && u.Scheme() != "https" {
		return nil, webError("WEB_INVALID_URL", fmt.Sprintf("unsupported URL scheme %q (only http and https are allowed)", u.Protocol()), nil)
	}
	if u.Username() != "" || u.Password() != "" {
		return nil, webError("WEB_BLOCKED_URL", "credentials in URLs are not allowed", nil)
	}
	if strings.HasSuffix(u.Hostname(), ".") {
		return nil, webError("WEB_BLOCKED_URL", "invalid web host", nil)
	}
	return u, nil
}

func isRedirect(status int) bool {
	switch status {
	case 301, 302, 303, 307, 308:
		return true
	default:
		return false
	}
}

func readBody(ctx context.Context, resp *http.Response, finalURL string) (web.FetchResult, error) {
	contentTypes := resp.Header.Values("Content-Type")
	contentType := strings.Join(contentTypes, ", ")
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	kind := "text"
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		kind = "html"
	case strings.HasPrefix(mediaType, "text/"), mediaType == "application/json", mediaType == "application/xml", strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
	default:
		if len(contentTypes) == 0 {
			contentType = "unknown"
		}
		return web.FetchResult{}, webError("WEB_UNSUPPORTED_CONTENT_TYPE", fmt.Sprintf("unsupported content type %q", contentType), nil)
	}
	decoder, fatalUTF8, err := responseDecoder(contentType)
	if err != nil {
		return web.FetchResult{}, err
	}
	if resp.ContentLength > maxResponseBytes {
		return web.FetchResult{}, webError("WEB_FETCH_TOO_LARGE", "response exceeds the maximum of 5000000 bytes", nil)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return web.FetchResult{}, fetchFailure(ctx, err)
	}
	truncatedBytes := len(data) > maxResponseBytes
	if truncatedBytes {
		data = data[:maxResponseBytes]
	}
	if fatalUTF8 && !utf8.Valid(data) {
		validPrefix := 0
		for validPrefix < len(data) {
			r, width := utf8.DecodeRune(data[validPrefix:])
			if r == utf8.RuneError && width == 1 {
				break
			}
			validPrefix += width
		}
		if truncatedBytes && !utf8.FullRune(data[validPrefix:]) {
			data = data[:validPrefix]
		} else {
			return web.FetchResult{}, webError("WEB_UNSUPPORTED_CONTENT_TYPE", "response body is not valid UTF-8 for the malformed charset declaration", nil)
		}
	}
	decoded, err := decoder.NewDecoder().Bytes(data)
	if err != nil {
		return web.FetchResult{}, webError("WEB_UNSUPPORTED_CONTENT_TYPE", "response body decoding failed", err)
	}
	text := string(decoded)
	if decoder == unicode.UTF8 || decoder == utf16LE || decoder == utf16BE {
		text = strings.TrimPrefix(text, "\uFEFF")
	}
	text, truncatedChars := truncateBodyChars(text)
	return web.FetchResult{
		URL: finalURL, StatusCode: resp.StatusCode,
		Body: web.FetchBody{Kind: kind, Content: text}, Truncated: truncatedBytes || truncatedChars,
	}, nil
}

var charsetParameter = regexp.MustCompile(`(?i);\s*charset\s*=\s*"?([^";]+)"?`)
var utf16LE = unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
var utf16BE = unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)

func responseDecoder(contentType string) (encoding.Encoding, bool, error) {
	match := charsetParameter.FindStringSubmatch(contentType)
	if match == nil {
		return unicode.UTF8, false, nil
	}
	label := strings.ToLower(strings.TrimFunc(match[1], web.IsSpace))
	if enc := textEncoding(label); enc != nil {
		return enc, false, nil
	}
	for _, token := range strings.FieldsFunc(label, func(r rune) bool { return r == ',' || web.IsSpace(r) }) {
		if textEncoding(token) == unicode.UTF8 {
			return unicode.UTF8, true, nil
		}
	}
	return nil, false, webError("WEB_UNSUPPORTED_CONTENT_TYPE", fmt.Sprintf("unsupported charset %q", label), nil)
}

func textEncoding(label string) encoding.Encoding {
	enc, name := charset.Lookup(label)
	switch name {
	case "utf-8":
		return unicode.UTF8
	case "utf-16le":
		return utf16LE
	case "utf-16be":
		return utf16BE
	case "replacement":
		return nil
	default:
		return enc
	}
}

func truncateBodyChars(text string) (string, bool) {
	units := 0
	for offset, character := range text {
		width := utf16.RuneLen(character)
		units += width
		if units > maxBodyChars {
			if width == 2 && units == maxBodyChars+1 {
				// An isolated UTF-16 surrogate becomes U+FFFD in UTF-8.
				return text[:offset] + "\uFFFD", true
			}
			return text[:offset], true
		}
	}
	return text, false
}

func utf16Length(text string) int {
	units := 0
	for _, character := range text {
		units += utf16.RuneLen(character)
	}
	return units
}

func fetchFailure(ctx context.Context, cause error) error {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return webError("WEB_FETCH_TIMEOUT", "web fetch timed out", cause)
	case context.Canceled:
		return webError("WEB_ABORTED", "web fetch aborted", cause)
	}
	var failure *web.Error
	if errors.As(cause, &failure) {
		return failure
	}
	return webError("WEB_PROVIDER_ERROR", "web fetch failed: TypeError: fetch failed", cause)
}

func webError(code, message string, cause error) error {
	return &web.Error{Code: code, Message: message, Cause: cause}
}
