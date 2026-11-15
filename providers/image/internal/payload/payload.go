package payload

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"

	"go-harness/thirdparty/providerhttp"
)

const maxBytes = 24 << 20
const ResponseLimit = (maxBytes+2)/3*4 + (256 << 10)

type Error struct{ Code, Message string }

func (e Error) Error() string                   { return e.Message }
func (e Error) ToolErrorInfo() (string, string) { return "ImageGenerationError", e.Code }

func Invalid(provider string) error {
	return Error{"IMAGE_GENERATION_RESPONSE", provider + " returned an invalid or oversized image."}
}

func RequestError(provider string, status int, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return Error{"IMAGE_GENERATION_TIMEOUT", provider + " timed out. The request may already have reached the provider."}
	case errors.Is(err, providerhttp.ErrResponse):
		return Invalid(provider)
	case err != nil:
		return Error{"IMAGE_GENERATION_NETWORK", provider + " could not complete the request. It may already have reached the provider."}
	}
	switch status {
	case http.StatusOK:
		return nil
	case 401, 403:
		return Error{"IMAGE_GENERATION_AUTH", provider + " rejected the API key or model access. Check Image generation settings."}
	case 402:
		return Error{"IMAGE_GENERATION_QUOTA", provider + " rejected the request for billing or quota reasons."}
	case 429:
		return Error{"IMAGE_GENERATION_RATE_LIMIT", provider + " rate limited the request or its quota was reached."}
	case 400, 404, 422:
		return Error{"IMAGE_GENERATION_REJECTED", provider + " rejected the image request. Check the selected model and prompt."}
	default:
		return Error{"IMAGE_GENERATION_UPSTREAM", fmt.Sprintf("%s could not complete the image request (HTTP %d).", provider, status)}
	}
}

func Decode(provider, data, mime string) ([]byte, string, error) {
	if data == "" || len(data) > base64.StdEncoding.EncodedLen(maxBytes) {
		return nil, "", Invalid(provider)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(raw) > maxBytes {
		return nil, "", Invalid(provider)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	detected := "image/" + format
	if err != nil || (format != "png" && format != "jpeg") || (mime != "" && mime != detected) ||
		cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 32_000_000 {
		return nil, "", Invalid(provider)
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		return nil, "", Invalid(provider)
	}
	return raw, detected, nil
}
