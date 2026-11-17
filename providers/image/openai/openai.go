package openai

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go-bot/harness/content"
	"go-bot/internal/providersettings"
	"go-bot/providers/image/internal/payload"
	"go-bot/thirdparty/openaiimage"
	"go-bot/thirdparty/providerhttp"
)

const ID = "openai"
const DefaultModel = "gpt-image-2"
const name = "OpenAI Images"

type Client struct {
	model, key, endpoint string
	http                 *http.Client
}

func New(model, key string) (*Client, error) {
	if !providersettings.ValidModel(model) || key == "" || !providersettings.ValidAPIKey(key) {
		return nil, errors.New("invalid OpenAI image model or API key")
	}
	return &Client{model, key, openaiimage.Endpoint, providerhttp.New(2 * time.Minute)}, nil
}

func (c *Client) Generate(ctx context.Context, prompt string, references ...content.ImageInput) ([]byte, string, error) {
	urls := make([]string, 0, len(references))
	for _, reference := range references {
		urls = append(urls, reference.DataURL())
	}
	res, status, err := openaiimage.Generate(ctx, c.http, c.endpoint, c.key, c.model, prompt, urls, payload.ResponseLimit)
	if err := payload.RequestError(name, status, err); err != nil {
		return nil, "", err
	}
	if len(res.Data) != 1 {
		return nil, "", payload.Invalid(name)
	}
	return payload.Decode(name, res.Data[0].Base64, "image/png")
}
