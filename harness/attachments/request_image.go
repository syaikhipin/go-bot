package attachments

import (
	"bytes"
	"context"
	"errors"
	"image"
	"math"
	"sync"

	"go-bot/harness/content"
)

// RequestImages applies a route's image budget to a verified durable source.
// Variants are derived, process-local data and never become attachment IDs.
type RequestImages struct {
	Store
	MaxWidth, MaxHeight, MaxBytes int

	mu         sync.Mutex
	cache      map[requestImageKey]requestImage
	order      []requestImageKey
	cacheBytes int
}

type requestImageKey struct {
	id                   string
	width, height, bytes int
}

type requestImage struct {
	data      []byte
	mediaType string
}

const maxRequestImageCacheBytes = 32 << 20
const maxRequestImageCacheEntries = 256

func (s *RequestImages) LoadRequestImage(ctx context.Context, ref content.ImageAttachment) ([]byte, string, error) {
	source, err := s.LoadImage(ctx, ref)
	if err != nil {
		return nil, "", err
	}
	if s.MaxWidth < 1 || s.MaxHeight < 1 || s.MaxBytes < 1 {
		return nil, "", errors.New("invalid model image variant target")
	}
	if ref.Width <= s.MaxWidth && ref.Height <= s.MaxHeight && len(source) <= s.MaxBytes {
		return source, ref.MIMEType, nil
	}
	key := requestImageKey{id: ref.ID, width: s.MaxWidth, height: s.MaxHeight, bytes: s.MaxBytes}
	s.mu.Lock()
	cached, found := s.cache[key]
	s.mu.Unlock()
	if found {
		return bytes.Clone(cached.data), cached.mediaType, nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		return nil, "", err
	}
	scale := min(1.0, float64(s.MaxWidth)/float64(ref.Width), float64(s.MaxHeight)/float64(ref.Height))
	if scale < 1 {
		width := max(1, int(math.Round(float64(ref.Width)*scale)))
		height := max(1, int(math.Round(float64(ref.Height)*scale)))
		decoded = resized(decoded, width, height)
	}
	data, mediaType, err := encodeNormalized(decoded, s.MaxBytes, encodedAlphaPlane(source, ref.MIMEType))
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if len(data) <= maxRequestImageCacheBytes {
		s.remember(key, requestImage{data: bytes.Clone(data), mediaType: mediaType})
	}
	return data, mediaType, nil
}

func (s *RequestImages) remember(key requestImageKey, variant requestImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = make(map[requestImageKey]requestImage)
	}
	if _, exists := s.cache[key]; exists {
		return
	}
	for s.cacheBytes+len(variant.data) > maxRequestImageCacheBytes || len(s.order) >= maxRequestImageCacheEntries {
		old := s.order[0]
		s.order = s.order[1:]
		s.cacheBytes -= len(s.cache[old].data)
		delete(s.cache, old)
	}
	s.cache[key] = variant
	s.order = append(s.order, key)
	s.cacheBytes += len(variant.data)
}
