package gemini

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"go-harness/thirdparty/providerhttp"
)

const Endpoint = "https://generativelanguage.googleapis.com/v1beta/models"

type Part struct {
	Thought    bool `json:"thought"`
	InlineData *struct {
		MIME string `json:"mimeType"`
		Data string `json:"data"`
	} `json:"inlineData"`
}

type Response struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
		Content      struct {
			Parts []Part `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

type InlineImage struct {
	MIMEType string `json:"mimeType"`
	Data     []byte `json:"data"`
}

func Generate(ctx context.Context, client *http.Client, endpoint, key, model, prompt string, references []InlineImage, limit int64) (Response, int, error) {
	parts := []map[string]any{{"text": prompt}}
	for _, image := range references {
		parts = append(parts, map[string]any{"inlineData": image})
	}
	req := map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": parts}},
		"generationConfig": map[string]any{"responseModalities": []string{"IMAGE"}, "candidateCount": 1},
	}
	target := strings.TrimRight(endpoint, "/") + "/" + url.PathEscape(model) + ":generateContent"
	var result Response
	status, err := providerhttp.Post(ctx, client, target, "x-goog-api-key", key, req, &result, limit)
	return result, status, err
}
