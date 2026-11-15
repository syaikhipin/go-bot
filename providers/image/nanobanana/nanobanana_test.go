package nanobanana

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-harness/harness/content"
	"go-harness/thirdparty/gemini"
)

func TestGenerate(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, reply, code string }{
		{"inline image", `{"candidates":[{"content":{"parts":[{"thought":true,"inlineData":{"mimeType":"image/jpeg","data":"ignored"}},{"text":"caption"},{"inlineData":{"mimeType":"image/jpeg","data":"` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `"}}]}}]}`, ""},
		{"text only", `{"candidates":[{"content":{"parts":[{"text":"caption"}]}}]}`, "IMAGE_GENERATION_RESPONSE"},
		{"prompt blocked", `{"promptFeedback":{"blockReason":"IMAGE_SAFETY"}}`, "IMAGE_GENERATION_REJECTED"},
		{"candidate blocked", `{"candidates":[{"finishReason":"IMAGE_PROHIBITED_CONTENT"}]}`, "IMAGE_GENERATION_REJECTED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Contents         []struct{ Parts []struct{ Text string } }
					GenerationConfig struct{ ResponseModalities []string }
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/v1beta/models/"+DefaultModel+":generateContent" || r.Method != http.MethodPost ||
					r.Header.Get("x-goog-api-key") != "fixture-secret" || r.URL.RawQuery != "" || len(body.Contents) != 1 || len(body.Contents[0].Parts) != 1 ||
					body.Contents[0].Parts[0].Text != "draw a blue square" || len(body.GenerationConfig.ResponseModalities) != 1 || body.GenerationConfig.ResponseModalities[0] != "IMAGE" {
					t.Error("unexpected Gemini request")
				}
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer server.Close()
			c, err := New("models/"+DefaultModel, "fixture-secret")
			if err != nil {
				t.Fatal(err)
			}
			c.endpoint = server.URL + "/v1beta/models"
			got, mime, err := c.Generate(t.Context(), "draw a blue square")
			if tc.code == "" {
				if err != nil || mime != "image/jpeg" || !bytes.Equal(got, encoded.Bytes()) {
					t.Fatalf("valid inline image not returned: %v", err)
				}
			} else {
				typed, ok := err.(interface{ ToolErrorInfo() (string, string) })
				if !ok {
					t.Fatalf("expected typed error, got %v", err)
				}
				if _, code := typed.ToolErrorInfo(); code != tc.code {
					t.Fatalf("unexpected error %s: %v", code, err)
				}
			}
		})
	}
}

func TestEditSendsOrderedInlineImages(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	references := []content.ImageInput{
		{MIMEType: "image/jpeg", Data: encoded.Bytes()},
		{MIMEType: "image/png", Data: []byte("second")},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Contents []struct {
				Parts []struct {
					Text       string
					InlineData gemini.InlineImage
				}
			}
			GenerationConfig struct{ CandidateCount int }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Contents) != 1 ||
			len(body.Contents[0].Parts) != 3 || body.Contents[0].Parts[0].Text != "edit" || body.GenerationConfig.CandidateCount != 1 {
			t.Error("unexpected Gemini edit request")
		} else {
			for i, ref := range references {
				inline := body.Contents[0].Parts[i+1].InlineData
				if inline.MIMEType != ref.MIMEType || !bytes.Equal(inline.Data, ref.Data) {
					t.Errorf("wrong inline data for reference %d", i)
				}
			}
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/jpeg","data":"` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `"}}]}}]}`))
	}))
	defer server.Close()
	c, err := New(DefaultModel, "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	c.endpoint = server.URL + "/v1beta/models"
	if got, mime, err := c.Generate(t.Context(), "edit", references...); err != nil || mime != "image/jpeg" || !bytes.Equal(got, encoded.Bytes()) {
		t.Fatalf("valid edited image not returned: %v", err)
	}
}
