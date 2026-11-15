package host

import (
	"context"
	"net/http"
	"sync"

	"go-harness/harness/content"
	"go-harness/internal/providersettings"
	imageproviders "go-harness/providers/image"
)

type hostImageGeneration struct {
	mu        sync.RWMutex
	provider  imageproviders.Generator
	selection providersettings.Selection
}

func (s *hostImageGeneration) Generate(ctx context.Context, _ string, prompt string, references ...content.ImageInput) ([]byte, string, error) {
	s.mu.RLock()
	provider := s.provider
	s.mu.RUnlock()
	if provider == nil {
		return nil, "", imageproviders.UnconfiguredError{}
	}
	return provider.Generate(ctx, prompt, references...)
}

func (s *hostImageGeneration) handle(w http.ResponseWriter, r *http.Request) {
	settings, ok := decodeProviderSelection(w, r)
	if !ok {
		return
	}
	if settings.Provider == "" {
		settings.Provider = imageproviders.Default
	}
	if !imageproviders.Supports(settings.Provider) || (settings.Model != "" && !providersettings.ValidModel(settings.Model)) {
		http.Error(w, "invalid image generation settings", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selection == settings {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var provider imageproviders.Generator
	if settings.APIKey != "" {
		var err error
		provider, err = imageproviders.New(settings.Provider, settings.Credential)
		if err != nil {
			http.Error(w, "invalid image generation settings", http.StatusBadRequest)
			return
		}
	}
	s.provider, s.selection = provider, settings
	w.WriteHeader(http.StatusNoContent)
}
