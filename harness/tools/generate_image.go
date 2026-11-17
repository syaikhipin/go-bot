package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/content"
)

// ImageGenerator keeps provider credentials and accounting outside the tool.
type ImageGenerator interface {
	Generate(context.Context, string, string, ...content.ImageInput) ([]byte, string, error)
}

type BudgetedImageGenerator struct {
	Inner ImageGenerator
	Max   int
	mu    sync.Mutex
	calls int
}

func (g *BudgetedImageGenerator) Generate(ctx context.Context, userID, prompt string, references ...content.ImageInput) ([]byte, string, error) {
	g.mu.Lock()
	if g.Inner == nil || g.Max <= 0 || g.calls >= g.Max {
		g.mu.Unlock()
		return nil, "", errors.New("image generation budget exhausted or unavailable")
	}
	g.calls++
	g.mu.Unlock()
	return g.Inner.Generate(ctx, userID, prompt, references...)
}

const maxReferenceImages = 10
const maxReferenceImageBytes = 12 << 20

type ImageReferenceReader interface {
	LoadReferenceImage(context.Context, string) (content.ImageInput, error)
}

type GenerateImage struct {
	Generator   ImageGenerator
	Attachments ImageAttachmentWriter
	References  ImageReferenceReader
	UserID      string
}

type generateImageArgs struct {
	Prompt       string   `json:"prompt"`
	ReferenceIDs []string `json:"reference_image_ids,omitempty"`
}

func (GenerateImage) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "generateimage",
		Description: "Generate one image from text, or edit an image or combine references. For image-to-image, pass the attachment IDs of the relevant uploaded, read, or generated images in reference_image_ids, in the order described by the prompt. Save and return one new image attachment.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"reference_image_ids":{"type":"array","items":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"maxItems":10,"description":"Optional image attachment IDs. Omit for text-to-image."}},"required":["prompt"]}`),
		Timeout:     5 * time.Minute,
	}
}

func decodeGenerateImageArgs(raw json.RawMessage) (generateImageArgs, error) {
	var args generateImageArgs
	if err := decodeOneToolObject(raw, &args); err != nil {
		return args, err
	}
	args.Prompt = strings.TrimSpace(args.Prompt)
	if args.Prompt == "" || len(args.Prompt) > 12000 {
		return args, errors.New("prompt must be nonempty and at most 12000 bytes")
	}
	if len(args.ReferenceIDs) > maxReferenceImages {
		return args, fmt.Errorf("at most %d image references are supported", maxReferenceImages)
	}
	return args, nil
}

func (t GenerateImage) Validate(raw json.RawMessage) error {
	if t.Generator == nil || t.Attachments == nil || t.UserID == "" {
		return errors.New("image generation is not configured")
	}
	args, err := decodeGenerateImageArgs(raw)
	if err != nil {
		return err
	}
	if len(args.ReferenceIDs) > 0 && t.References == nil {
		return errors.New("image reference storage is unavailable")
	}
	return nil
}

func (t GenerateImage) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	text, _, err := t.ExecuteContent(ctx, raw)
	return text, err
}

func (t GenerateImage) ExecuteContent(ctx context.Context, raw json.RawMessage) (string, []content.ContentBlock, error) {
	if err := t.Validate(raw); err != nil {
		return "", nil, err
	}
	args, _ := decodeGenerateImageArgs(raw)
	references := make([]content.ImageInput, 0, len(args.ReferenceIDs))
	total := 0
	for i, id := range args.ReferenceIDs {
		input, err := t.References.LoadReferenceImage(ctx, id)
		if err != nil {
			return "", nil, fmt.Errorf("could not load reference image %d: %w", i+1, err)
		}
		total += len(input.Data)
		if total > maxReferenceImageBytes {
			return "", nil, errors.New("reference images exceed the 12 MiB combined byte limit")
		}
		references = append(references, input)
	}
	body, mimeType, err := t.Generator.Generate(ctx, t.UserID, args.Prompt, references...)
	if err != nil {
		return "", nil, err
	}
	ref, err := t.Attachments.SaveImage(ctx, body, mimeType, "generated-image", "")
	if err != nil {
		return "", nil, fmt.Errorf("image was generated but could not be saved as an attachment: %w", err)
	}
	text := fmt.Sprintf("Generated image attachment %s (%s, %dx%d px, %d bytes).", ref.ID, ref.MIMEType, ref.Width, ref.Height, ref.Bytes)
	return text, []content.ContentBlock{content.ImageBlock(ref)}, nil
}
