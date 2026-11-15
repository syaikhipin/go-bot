package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go-harness/internal/webmarkdown"
	"go-harness/web"
)

type fakeWebFetcher struct {
	result   web.FetchResult
	requests []string
}

func (f *fakeWebFetcher) Fetch(_ context.Context, url string) (web.FetchResult, error) {
	f.requests = append(f.requests, url)
	return f.result, nil
}

func TestWebFetchTool(t *testing.T) {
	fetcher := &fakeWebFetcher{result: web.FetchResult{
		URL: "https://example.com/final", StatusCode: 200,
		Body: web.FetchBody{Kind: "text", Content: "page data"},
	}}
	tool := WebFetch{Fetcher: fetcher}
	if tool.Spec().Name != "web_fetch" {
		t.Fatal("unexpected tool name")
	}
	for _, raw := range []string{`{"url":""}`, `{"url":"   "}`, `{"url":"https://example.com"}{"url":"https://second.example.com"}`} {
		if err := tool.Validate(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted invalid arguments %s", raw)
		}
	}
	longURL, err := json.Marshal(map[string]string{"url": "https://example.com/" + strings.Repeat("😀", 1100)})
	if err != nil || tool.Validate(longURL) != nil {
		t.Fatalf("long URL should reach the fetch provider's validator: %v", err)
	}
	raw := json.RawMessage(`{"url":"https://example.com/start"}`)
	output, meta, err := tool.ExecutePresented(t.Context(), raw)
	if err != nil || len(fetcher.requests) != 1 || fetcher.requests[0] != "https://example.com/start" ||
		output != "Fetched https://example.com/final (HTTP 200)\n\n"+webTrustNotice+"\n\npage data" ||
		string(meta) != `{"url":"https://example.com/final","statusCode":200,"truncated":false}` {
		t.Fatalf("web fetch output=%q meta=%s requests=%v error=%v", output, meta, fetcher.requests, err)
	}
	if tool.Spec().Timeout != 30*time.Second || !tool.Spec().ParallelSafe {
		t.Fatalf("web fetch execution policy = %+v", tool.Spec())
	}
}

func TestWebFetchBoundsWholeOutputAndExplainsTruncation(t *testing.T) {
	const footer = "\n\n(Content truncated. Fetch a more specific URL or section for the full text.)"
	for _, page := range []web.FetchResult{
		{URL: "https://example.com", StatusCode: 200, Body: web.FetchBody{Kind: "text", Content: strings.Repeat("界", webOutputMaxChars)}},
		{URL: "https://example.com", StatusCode: 200, Body: web.FetchBody{Kind: "text", Content: strings.Repeat("😀", webOutputMaxChars)}},
		{URL: "https://example.com", StatusCode: 200, Body: web.FetchBody{Kind: "text", Content: "short text"}, Truncated: true},
	} {
		output, truncated, err := webmarkdown.Render(t.Context(), page, webOutputMaxChars)
		if err != nil || !truncated || utf16Length(output) > webOutputMaxChars || !utf8.ValidString(output) ||
			!strings.HasPrefix(output, "Fetched https://example.com (HTTP 200)\n\n"+webTrustNotice) || !strings.HasSuffix(output, footer) {
			t.Fatalf("bounded web output = %d chars, truncated=%t, err=%v", utf16Length(output), truncated, err)
		}
	}
}

func TestWebFetchSpillsLongModelResultAndKeepsMetadata(t *testing.T) {
	page := web.FetchResult{URL: "https://example.com/", StatusCode: 200,
		Body: web.FetchBody{Kind: "text", Content: "heading " + strings.Repeat("🧪", 32_000) + " tail"}}
	tool := WebFetch{Fetcher: &fakeWebFetcher{result: page}, SpillWorkdir: t.TempDir()}
	args := json.RawMessage(`{"url":"https://example.com/"}`)
	full, err := tool.Execute(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	preview, meta, err := tool.ExecutePresented(t.Context(), args)
	if err != nil || webTextTokens(preview) > webInlineTokens || !utf8.ValidString(preview) ||
		!strings.Contains(preview, "\n\n[...]\n\n") || !strings.Contains(preview, " tail\n\n(Omitted ") ||
		string(meta) != `{"url":"https://example.com/","statusCode":200,"truncated":false}` {
		t.Fatalf("spill preview tokens=%d meta=%s error=%v", webTextTokens(preview), meta, err)
	}
	files, err := filepath.Glob(filepath.Join(tool.SpillWorkdir, ".agent", "web-spill", "*-web_fetch.txt"))
	if err != nil || len(files) != 1 || !strings.Contains(preview, "Full formatted result stored at: "+files[0]+".") {
		t.Fatalf("spill file missing from preview: %v, %v", files, err)
	}
	saved, err := os.ReadFile(files[0])
	if err != nil || string(saved) != full {
		t.Fatal("spill does not contain the exact formatted tool output")
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("spill file is not private")
	}
}
