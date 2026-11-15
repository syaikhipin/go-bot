package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go-harness/harness/content"
)

func TestGenerate(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		status int
		reply  string
		code   string
	}{
		{"png", 200, `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `"}]}`, ""},
		{"invalid image", 200, `{"data":[{"b64_json":"aGVsbG8="}]}`, "IMAGE_GENERATION_RESPONSE"},
		{"bad key", 401, `{"error":{"message":"fixture-secret"}}`, "IMAGE_GENERATION_AUTH"},
		{"rate limit", 429, "", "IMAGE_GENERATION_RATE_LIMIT"},
		{"redirect", 307, "", "IMAGE_GENERATION_UPSTREAM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model  string `json:"model"`
					Prompt string `json:"prompt"`
					Count  int    `json:"n"`
					Format string `json:"output_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/v1/images/generations" || r.Method != http.MethodPost ||
					r.Header.Get("Authorization") != "Bearer fixture-secret" || body.Model != DefaultModel || body.Prompt != "draw a blue square" || body.Count != 1 || body.Format != "png" {
					t.Error("unexpected OpenAI request")
				}
				if tc.status == 307 {
					w.Header().Set("Location", "/must-not-follow")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer server.Close()
			c, err := New(DefaultModel, "fixture-secret")
			if err != nil {
				t.Fatal(err)
			}
			c.endpoint = server.URL + "/v1/images/generations"
			got, mime, err := c.Generate(t.Context(), "draw a blue square")
			if tc.code == "" {
				if err != nil || mime != "image/png" || !bytes.Equal(got, encoded.Bytes()) {
					t.Fatalf("valid image not returned: %v", err)
				}
			} else {
				typed, ok := err.(interface{ ToolErrorInfo() (string, string) })
				if !ok {
					t.Fatalf("expected typed error, got %v", err)
				}
				_, code := typed.ToolErrorInfo()
				if code != tc.code || strings.Contains(err.Error(), "fixture-secret") {
					t.Fatalf("unexpected error %s: %v", code, err)
				}
			}
		})
	}
}

func TestEditSendsOrderedReferences(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	references := []content.ImageInput{
		{MIMEType: "image/png", Data: encoded.Bytes()},
		{MIMEType: "image/jpeg", Data: []byte("second")},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string
			N      int
			Images []struct {
				URL string `json:"image_url"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/v1/images/edits" ||
			r.Header.Get("Authorization") != "Bearer fixture-secret" || body.Prompt != "edit" || body.N != 1 {
			t.Error("unexpected OpenAI edit request")
		}
		var urls []string
		for _, item := range body.Images {
			urls = append(urls, item.URL)
		}
		if !slices.Equal(urls, []string{references[0].DataURL(), references[1].DataURL()}) {
			t.Error("reference images were missing or out of order")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string][]byte{{"b64_json": encoded.Bytes()}}})
	}))
	defer server.Close()
	c, err := New(DefaultModel, "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	c.endpoint = server.URL + "/v1/images/generations"
	if got, mime, err := c.Generate(t.Context(), "edit", references...); err != nil || mime != "image/png" || !bytes.Equal(got, encoded.Bytes()) {
		t.Fatalf("valid edited image not returned: %v", err)
	}
}
