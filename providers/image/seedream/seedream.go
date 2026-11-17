// Package seedream calls the Volcengine Ark single-image generation API.
package seedream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"time"

	"go-bot/harness/content"
	"go-bot/internal/providersettings"
)

const (
	ID               = "seedream"
	DefaultModel     = "doubao-seedream-5-0-260128"
	endpoint         = "https://ark.cn-beijing.volces.com/api/v3/images/generations"
	maxImageBytes    = 24 << 20
	maxResponseBytes = (maxImageBytes+2)/3*4 + (64 << 10)
)

type Error struct {
	Code, Message string
}

func (e *Error) Error() string                   { return e.Message }
func (e *Error) ToolErrorInfo() (string, string) { return "ImageGenerationError", e.Code }

type Client struct {
	model, apiKey, endpoint string
	http                    *http.Client
}

func New(model, apiKey string) (*Client, error) {
	if !providersettings.ValidModel(model) || apiKey == "" || !providersettings.ValidAPIKey(apiKey) {
		return nil, errors.New("invalid Seedream model or API key")
	}
	return &Client{model: model, apiKey: apiKey, endpoint: endpoint, http: &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Generate(ctx context.Context, prompt string, references ...content.ImageInput) ([]byte, string, error) {
	input := map[string]any{
		"model": c.model, "prompt": prompt, "size": "2K", "response_format": "b64_json",
	}
	if len(references) > 0 {
		urls := make([]string, 0, len(references))
		for _, reference := range references {
			urls = append(urls, reference.DataURL())
		}
		input["image"] = urls
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, "", &Error{"IMAGE_GENERATION_TIMEOUT", "Seedream timed out. The request may have reached Ark."}
		}
		return nil, "", &Error{"IMAGE_GENERATION_NETWORK", "Seedream request failed before a response was received. The request may have reached Ark."}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", providerError(res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return nil, "", &Error{"IMAGE_GENERATION_NETWORK", "Seedream response could not be read."}
	}
	var result struct {
		Data []struct {
			Image string `json:"b64_json"`
		} `json:"data"`
	}
	if len(data) > maxResponseBytes || json.Unmarshal(data, &result) != nil || len(result.Data) != 1 {
		return nil, "", &Error{"IMAGE_GENERATION_RESPONSE", "Seedream returned an invalid or oversized image response."}
	}
	encoded := result.Data[0].Image
	if len(encoded) == 0 || base64.StdEncoding.DecodedLen(len(encoded)) > maxImageBytes+2 {
		return nil, "", &Error{"IMAGE_GENERATION_RESPONSE", "Seedream returned an empty or oversized image."}
	}
	picture, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(picture) > maxImageBytes {
		return nil, "", &Error{"IMAGE_GENERATION_RESPONSE", "Seedream returned invalid or oversized image data."}
	}
	dimensions, format, err := image.DecodeConfig(bytes.NewReader(picture))
	if err != nil || (format != "jpeg" && format != "png") ||
		dimensions.Width <= 0 || dimensions.Height <= 0 || dimensions.Width > 8192 || dimensions.Height > 8192 ||
		int64(dimensions.Width)*int64(dimensions.Height) > 32_000_000 {
		return nil, "", &Error{"IMAGE_GENERATION_RESPONSE", "Seedream returned an unsupported image format or dimensions."}
	}
	if _, _, err := image.Decode(bytes.NewReader(picture)); err != nil {
		return nil, "", &Error{"IMAGE_GENERATION_RESPONSE", "Seedream returned a corrupt image."}
	}
	return picture, "image/" + format, nil
}

func providerError(status int) error {
	switch status {
	case 401, 403:
		return &Error{"IMAGE_GENERATION_AUTH", "Ark rejected the API key or access to the selected model. Check Image generation settings."}
	case 402:
		return &Error{"IMAGE_GENERATION_QUOTA", "Ark rejected the request for billing or quota reasons."}
	case 429:
		return &Error{"IMAGE_GENERATION_RATE_LIMIT", "Ark rate limited the image request or its quota was reached."}
	case 400, 404, 422:
		return &Error{"IMAGE_GENERATION_REJECTED", fmt.Sprintf("Ark rejected the image request (HTTP %d). Check the selected model or endpoint and the prompt.", status)}
	default:
		return &Error{"IMAGE_GENERATION_UPSTREAM", fmt.Sprintf("Ark could not complete the image request (HTTP %d).", status)}
	}
}
