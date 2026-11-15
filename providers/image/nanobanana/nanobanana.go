package nanobanana

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go-harness/harness/content"
	"go-harness/internal/providersettings"
	"go-harness/providers/image/internal/payload"
	"go-harness/thirdparty/gemini"
	"go-harness/thirdparty/providerhttp"
)

const ID = "nanobanana"
const DefaultModel = "gemini-3.1-flash-image"
const name = "Google Nano Banana"

type Client struct {
	model, key, endpoint string
	http                 *http.Client
}

func New(model, key string) (*Client, error) {
	model = strings.TrimPrefix(model, "models/")
	if !providersettings.ValidModel(model) || key == "" || !providersettings.ValidAPIKey(key) {
		return nil, errors.New("invalid Gemini image model or API key")
	}
	return &Client{model, key, gemini.Endpoint, providerhttp.New(2 * time.Minute)}, nil
}

func (c *Client) Generate(ctx context.Context, prompt string, references ...content.ImageInput) ([]byte, string, error) {
	inputs := make([]gemini.InlineImage, 0, len(references))
	for _, reference := range references {
		inputs = append(inputs, gemini.InlineImage{MIMEType: reference.MIMEType, Data: reference.Data})
	}
	res, status, err := gemini.Generate(ctx, c.http, c.endpoint, c.key, c.model, prompt, inputs, payload.ResponseLimit)
	if err := payload.RequestError(name, status, err); err != nil {
		return nil, "", err
	}
	if res.PromptFeedback.BlockReason != "" {
		return nil, "", blocked()
	}
	for _, candidate := range res.Candidates {
		switch candidate.FinishReason {
		case "SAFETY", "IMAGE_SAFETY", "PROHIBITED_CONTENT", "IMAGE_PROHIBITED_CONTENT", "BLOCKLIST", "IMAGE_RECITATION", "RECITATION":
			return nil, "", blocked()
		}
		for _, part := range candidate.Content.Parts {
			if part.InlineData != nil && !part.Thought {
				return payload.Decode(name, part.InlineData.Data, part.InlineData.MIME)
			}
		}
	}
	return nil, "", payload.Invalid(name)
}

func blocked() error {
	return payload.Error{Code: "IMAGE_GENERATION_REJECTED", Message: "Google Nano Banana blocked the image request."}
}
