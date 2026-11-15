package openaiimage

import (
	"context"
	"net/http"
	"strings"

	"go-harness/thirdparty/providerhttp"
)

const Endpoint = "https://api.openai.com/v1/images/generations"

type Response struct {
	Data []struct {
		Base64 string `json:"b64_json"`
	} `json:"data"`
}

func Generate(ctx context.Context, client *http.Client, endpoint, key, model, prompt string, references []string, limit int64) (Response, int, error) {
	req := struct {
		Model        string              `json:"model"`
		Prompt       string              `json:"prompt"`
		Count        int                 `json:"n"`
		OutputFormat string              `json:"output_format"`
		Images       []map[string]string `json:"images,omitempty"`
	}{Model: model, Prompt: prompt, Count: 1, OutputFormat: "png"}
	if len(references) > 0 {
		endpoint = strings.TrimSuffix(endpoint, "/generations") + "/edits"
		for _, url := range references {
			req.Images = append(req.Images, map[string]string{"image_url": url})
		}
	}
	var result Response
	status, err := providerhttp.Post(ctx, client, endpoint, "Authorization", "Bearer "+key, req, &result, limit)
	return result, status, err
}
