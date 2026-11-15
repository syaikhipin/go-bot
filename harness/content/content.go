package content

import (
	"encoding/base64"
	"errors"
)

// These values are owned by the ServerAgents harness, independently of the
// client-facing agentprotocol and the retired single-Session journal.
type ImageDimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ImageInput contains validated image bytes for a provider request.
type ImageInput struct {
	Data     []byte
	MIMEType string
}

func (i ImageInput) DataURL() string {
	return "data:" + i.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(i.Data)
}

// ImageAttachment identifies immutable image bytes stored outside a Session
// journal. Offload decisions target occurrences, not this identity.
type ImageAttachment struct {
	ID                 string           `json:"id"`
	MIMEType           string           `json:"mimeType"`
	Bytes              int64            `json:"bytes"`
	Width              int              `json:"width"`
	Height             int              `json:"height"`
	Name               string           `json:"name,omitempty"`
	ReadPath           string           `json:"readPath,omitempty"`
	OriginalDimensions *ImageDimensions `json:"originalDimensions,omitempty"`
}

type ContentBlock struct {
	Type               string           `json:"type"`
	Text               string           `json:"text,omitempty"`
	ImageRefID         string           `json:"imageRefId,omitempty"`
	MIMEType           string           `json:"mimeType,omitempty"`
	Bytes              int64            `json:"bytes,omitempty"`
	Width              int              `json:"width,omitempty"`
	Height             int              `json:"height,omitempty"`
	Name               string           `json:"name,omitempty"`
	ReadPath           string           `json:"readPath,omitempty"`
	OriginalDimensions *ImageDimensions `json:"originalDimensions,omitempty"`
}

func ImageBlock(ref ImageAttachment) ContentBlock {
	return ContentBlock{
		Type: "image", ImageRefID: ref.ID, MIMEType: ref.MIMEType,
		Bytes: ref.Bytes, Width: ref.Width, Height: ref.Height,
		Name: ref.Name, ReadPath: ref.ReadPath, OriginalDimensions: ref.OriginalDimensions,
	}
}

func ImageAttachmentFromBlock(block ContentBlock) ImageAttachment {
	return ImageAttachment{
		ID: block.ImageRefID, MIMEType: block.MIMEType, Bytes: block.Bytes,
		Width: block.Width, Height: block.Height, Name: block.Name, ReadPath: block.ReadPath,
		OriginalDimensions: block.OriginalDimensions,
	}
}

func CloneContentBlocks(blocks []ContentBlock) []ContentBlock {
	copy := append([]ContentBlock(nil), blocks...)
	for index := range copy {
		if copy[index].OriginalDimensions != nil {
			dimensions := *copy[index].OriginalDimensions
			copy[index].OriginalDimensions = &dimensions
		}
	}
	return copy
}

// The model adapter and attachment reader validate image metadata before use.
func ValidateDurableContentBlocks(blocks []ContentBlock) error {
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == "" || block.ImageRefID != "" {
				return errors.New("invalid text content block")
			}
		case "image":
			if block.ImageRefID == "" || block.Text != "" {
				return errors.New("invalid image content block")
			}
			if block.OriginalDimensions != nil && (block.OriginalDimensions.Width <= 0 || block.OriginalDimensions.Height <= 0) {
				return errors.New("invalid original image dimensions")
			}
		default:
			return errors.New("unknown content block type")
		}
	}
	return nil
}
