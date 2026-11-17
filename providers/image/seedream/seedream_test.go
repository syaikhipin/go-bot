package seedream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go-bot/harness/content"
)

func TestGenerateArkRequestAndResponse(t *testing.T) {
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	requests := make(chan []string, 2)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string
			Prompt string
			Size   string
			Images []string `json:"image"`
			Format string   `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/images/generations" ||
			r.Header.Get("Authorization") != "Bearer fixture-key" || body.Model != "ep-fixture" ||
			body.Prompt != "a blue flower" || body.Size != "2K" || body.Format != "b64_json" {
			t.Errorf("unexpected Ark request: %s %s %v", r.Method, r.URL.Path, body)
		}
		requests <- body.Images
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"b64_json": base64.StdEncoding.EncodeToString(img.Bytes())},
		}})
	}))
	defer fixture.Close()
	c, err := New("ep-fixture", "fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	c.endpoint = fixture.URL + "/api/v3/images/generations"
	references := []content.ImageInput{{MIMEType: "image/png", Data: img.Bytes()}, {MIMEType: "image/webp", Data: []byte("second")}}
	for _, inputs := range [][]content.ImageInput{nil, references} {
		picture, mime, err := c.Generate(t.Context(), "a blue flower", inputs...)
		if err != nil || mime != "image/png" || !bytes.Equal(picture, img.Bytes()) {
			t.Fatalf("Generate returned %d bytes, %q, %v", len(picture), mime, err)
		}
		var want []string
		for _, input := range inputs {
			want = append(want, input.DataURL())
		}
		if got := <-requests; !slices.Equal(got, want) {
			t.Fatalf("unexpected Ark reference images: %v", got)
		}
	}
}

func TestGenerateArkFailures(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		reply func(http.ResponseWriter, *http.Request)
	}{
		{"authentication", "IMAGE_GENERATION_AUTH", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "fixture-secret", 401) }},
		{"billing", "IMAGE_GENERATION_QUOTA", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "fixture-secret", 402) }},
		{"throttling", "IMAGE_GENERATION_RATE_LIMIT", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "fixture-secret", 429) }},
		{"invalid image", "IMAGE_GENERATION_RESPONSE", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"b64_json":"aGVsbG8="}]}`))
		}},
		{"oversized response", "IMAGE_GENERATION_RESPONSE", func(w http.ResponseWriter, _ *http.Request) {
			chunk := bytes.Repeat([]byte{' '}, 64<<10)
			for total := 0; total <= maxResponseBytes; total += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}},
		{"deadline", "IMAGE_GENERATION_TIMEOUT", func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := httptest.NewServer(http.HandlerFunc(tc.reply))
			defer fixture.Close()
			c, err := New(DefaultModel, "fixture-key")
			if err != nil {
				t.Fatal(err)
			}
			c.endpoint = fixture.URL
			ctx := t.Context()
			if tc.name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			_, _, err = c.Generate(ctx, "test")
			var providerErr *Error
			if !errors.As(err, &providerErr) || providerErr.Code != tc.code || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("unexpected provider error: %v", err)
			}
		})
	}
}
