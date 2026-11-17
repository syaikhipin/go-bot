package agent

import (
	"context"
	"encoding/base64"
	"errors"

	"go-bot/harness/content"
)

type ImageAttachmentReader interface {
	LoadImage(context.Context, content.ImageAttachment) ([]byte, error)
}

const (
	defaultMaxInlineImageBytes = 15 << 20
	defaultMaxImages           = 30
	defaultInlineByteQuantum   = 5 << 20
	defaultImageCountQuantum   = 5
)

type imageBudget struct {
	maxBytes     int64
	maxImages    int
	byteQuantum  int64
	countQuantum int
}

type preparedImage struct {
	attachment content.ImageAttachment
	data       []byte
}

// Count the oldest occurrences required by the inline route's exact base64
// payload length. The data URL prefix is transport framing, not image bytes.
// DeepSeek rounds the removal target up by byte and count quanta; for a
// non-unit byte quantum it removes past the rounded target, leaving headroom.
func imagesToOffload(images []preparedImage, budget imageBudget) (int, error) {
	if budget.maxBytes == 0 {
		budget.maxBytes = defaultMaxInlineImageBytes
	}
	if budget.maxImages == 0 {
		budget.maxImages = defaultMaxImages
	}
	if budget.byteQuantum == 0 {
		budget.byteQuantum = defaultInlineByteQuantum
	}
	if budget.countQuantum == 0 {
		budget.countQuantum = defaultImageCountQuantum
	}
	if budget.maxBytes < 1 || budget.maxImages < 1 || budget.byteQuantum < 1 || budget.countQuantum < 1 ||
		budget.byteQuantum > budget.maxBytes || budget.countQuantum > budget.maxImages {
		return 0, errors.New("invalid inline image budget or offload quantum")
	}
	lengths := make([]int64, 0, len(images))
	var total int64
	for _, image := range images {
		length := int64(base64.StdEncoding.EncodedLen(len(image.data)))
		lengths = append(lengths, length)
		total += length
	}
	excessCount := max(0, len(images)-budget.maxImages)
	excessBytes := max(int64(0), total-budget.maxBytes)
	if excessCount == 0 && excessBytes == 0 {
		return 0, nil
	}
	removeCount := ((excessCount + budget.countQuantum - 1) / budget.countQuantum) * budget.countQuantum
	removeBytes := ((excessBytes + budget.byteQuantum - 1) / budget.byteQuantum) * budget.byteQuantum
	count, removedBytes := 0, int64(0)
	for _, length := range lengths {
		byteTargetMet := removeBytes == 0 ||
			(budget.byteQuantum == 1 && removedBytes >= removeBytes) ||
			(budget.byteQuantum > 1 && removedBytes > removeBytes)
		if count >= removeCount && byteTargetMet {
			break
		}
		count++
		removedBytes += length
	}
	return count, nil
}
