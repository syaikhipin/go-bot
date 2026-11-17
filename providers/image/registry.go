// Package imageproviders registers the image generators bundled with the Host.
package imageproviders

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go-bot/harness/content"
	"go-bot/internal/providersettings"
	"go-bot/providers/image/nanobanana"
	"go-bot/providers/image/openai"
	"go-bot/providers/image/seedream"
)

const Default = seedream.ID
const InternalPath = "/internal/host/image-generation"

type Generator interface {
	Generate(context.Context, string, ...content.ImageInput) ([]byte, string, error)
}

type Adapter struct {
	Descriptor providersettings.Descriptor
	New        func(providersettings.Credential) (Generator, error)
}

var adapters = map[string]Adapter{
	seedream.ID: {
		Descriptor: providersettings.Descriptor{ID: seedream.ID, Name: "Seedream", DefaultModel: seedream.DefaultModel},
		New: func(config providersettings.Credential) (Generator, error) {
			return seedream.New(config.Model, config.APIKey)
		},
	},
	openai.ID: {
		Descriptor: providersettings.Descriptor{ID: openai.ID, Name: "OpenAI Images", DefaultModel: openai.DefaultModel},
		New: func(config providersettings.Credential) (Generator, error) {
			return openai.New(config.Model, config.APIKey)
		},
	},
	nanobanana.ID: {
		Descriptor: providersettings.Descriptor{ID: nanobanana.ID, Name: "Google Nano Banana", DefaultModel: nanobanana.DefaultModel},
		New: func(config providersettings.Credential) (Generator, error) {
			return nanobanana.New(config.Model, config.APIKey)
		},
	},
}

func Catalog() []providersettings.Descriptor {
	result := make([]providersettings.Descriptor, 0, len(adapters))
	for _, adapter := range adapters {
		result = append(result, adapter.Descriptor)
	}
	slices.SortFunc(result, func(a, b providersettings.Descriptor) int {
		return strings.Compare(a.ID, b.ID)
	})
	return result
}

func New(id string, config providersettings.Credential) (Generator, error) {
	adapter, ok := adapters[id]
	if !ok {
		return nil, errors.New("unsupported image generation provider")
	}
	return adapter.New(config)
}

func Supports(id string) bool {
	_, ok := adapters[id]
	return ok
}
