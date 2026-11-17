package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"go-bot/harness/agent"
	"go-bot/harness/content"
)

const maxReadImageSourceBytes = 20 << 20

type ImageAttachmentWriter interface {
	SaveImage(context.Context, []byte, string, string, string) (content.ImageAttachment, error)
}

// ReadImage returns the actual image to the primary model on its next step.
// It does not make a separate visual-model call or synthesize a description.
type ReadImage struct {
	View              FileView
	Attachments       ImageAttachmentWriter
	ImageInputEnabled bool
}

type readImageArgs struct {
	FilePath string `json:"file_path"`
}

func (ReadImage) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "readimage", Description: "Read a PNG, JPEG, WebP, or GIF file and return its pixels to the current image-capable model. Large images are normalized automatically; do not create thumbnails just to inspect an image.", Parameters: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`), ParallelSafe: true}
}

func decodeReadImageArgs(raw json.RawMessage) (readImageArgs, error) {
	var args readImageArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&args); err != nil {
		return args, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return args, errors.New("readimage requires one JSON object")
	}
	args.FilePath = strings.TrimSpace(args.FilePath)
	if args.FilePath == "" || len(args.FilePath) > 4096 || strings.ContainsRune(args.FilePath, 0) {
		return args, errors.New("file_path must be a nonempty file path at most 4096 bytes")
	}
	return args, nil
}

func (t ReadImage) Validate(raw json.RawMessage) error {
	if t.View == nil || t.Attachments == nil {
		return errors.New("durable image attachments are not configured")
	}
	if !t.ImageInputEnabled {
		return errors.New("current model route does not declare image input")
	}
	_, err := decodeReadImageArgs(raw)
	return err
}

func (t ReadImage) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	content, _, err := t.ExecuteContent(ctx, raw)
	return content, err
}

func (t ReadImage) ExecuteContent(ctx context.Context, raw json.RawMessage) (string, []content.ContentBlock, error) {
	if err := t.Validate(raw); err != nil {
		return "", nil, err
	}
	args, _ := decodeReadImageArgs(raw)
	var declaredMIME string
	switch strings.ToLower(filepath.Ext(args.FilePath)) {
	case ".png":
		declaredMIME = "image/png"
	case ".jpg", ".jpeg":
		declaredMIME = "image/jpeg"
	case ".webp":
		declaredMIME = "image/webp"
	case ".gif":
		declaredMIME = "image/gif"
	case "":
	default:
		return "", nil, errors.New("file extension does not declare PNG, JPEG, WebP, or GIF")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	read, err := t.View.ReadFile(args.FilePath, maxReadImageSourceBytes)
	if err != nil {
		return "", nil, err
	}
	if read.Truncated {
		return "", nil, errors.New("image exceeds the 20 MiB source byte limit")
	}
	displayPath := read.ResolvedPath
	if displayPath == "" {
		displayPath = args.FilePath
	}
	ref, err := t.Attachments.SaveImage(ctx, read.Content, declaredMIME, filepath.Base(displayPath), displayPath)
	if err != nil {
		return "", nil, err
	}
	output := formatImageReadOutput(displayPath, ref)
	return output, []content.ContentBlock{content.ImageBlock(ref)}, nil
}

func formatImageReadOutput(path string, ref content.ImageAttachment) string {
	scaled := ""
	if original := ref.OriginalDimensions; original != nil && ref.Width > 0 && ref.Height > 0 {
		x := fmt.Sprintf("%.2f", float64(original.Width)/float64(ref.Width))
		y := fmt.Sprintf("%.2f", float64(original.Height)/float64(ref.Height))
		advice := "multiply coordinates by " + x
		if x != y {
			advice = "multiply x coordinates by " + x + " and y coordinates by " + y
		}
		scaled = fmt.Sprintf(" (downscaled from %dx%d px; %s to locate features in the original file)", original.Width, original.Height, advice)
	}
	return fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\n%s image, %dx%d px, %d bytes%s\n</content>", path, ref.MIMEType, ref.Width, ref.Height, ref.Bytes, scaled)
}
