package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"

	"go-bot/harness/attachments"
	"go-bot/harness/content"
)

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

type recordingImageGenerator struct {
	calls int
	user  string
	bytes []byte
	mime  string
	refs  []content.ImageInput
}

func (g *recordingImageGenerator) Generate(_ context.Context, userID, _ string, references ...content.ImageInput) ([]byte, string, error) {
	g.calls++
	g.user = userID
	g.refs = references
	return g.bytes, g.mime, nil
}

func TestGenerateImageStoresAttachmentWithoutWorkspaceFile(t *testing.T) {
	root := t.TempDir()
	imageBytes := tinyPNG(t)
	generator := &recordingImageGenerator{bytes: imageBytes, mime: "image/png"}
	store := attachments.Store{API: attachments.LocalObjectAPI{Root: root}}
	tool := GenerateImage{Generator: generator, Attachments: store, UserID: "alice"}

	if _, _, err := tool.ExecuteContent(t.Context(), json.RawMessage(`{"prompt":" "}`)); err == nil || generator.calls != 0 {
		t.Fatalf("empty prompt reached image provider: calls=%d err=%v", generator.calls, err)
	}
	result, blocks, err := tool.ExecuteContent(t.Context(), json.RawMessage(`{"prompt":"draw"}`))
	if err != nil || generator.calls != 1 || generator.user != "alice" {
		t.Fatalf("image tool result=%q calls=%d user=%q err=%v", result, generator.calls, generator.user, err)
	}
	if len(blocks) != 1 || blocks[0].Type != "image" || blocks[0].Name != "generated-image" ||
		blocks[0].ReadPath != "" || !strings.Contains(result, blocks[0].ImageRefID) {
		t.Fatalf("generated image result = %q, %#v", result, blocks)
	}
	data, err := store.LoadImage(t.Context(), content.ImageAttachmentFromBlock(blocks[0]))
	if err != nil || !bytes.Equal(data, imageBytes) {
		t.Fatalf("generated attachment data = %d bytes, %v", len(data), err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "attachments" {
		t.Fatalf("expected only the attachment store, found %v: %v", entries, err)
	}
}

func TestBudgetedImageGeneratorLimitsCalls(t *testing.T) {
	inner := &recordingImageGenerator{bytes: tinyPNG(t), mime: "image/png"}
	limited := &BudgetedImageGenerator{Inner: inner, Max: 1}
	if _, _, err := limited.Generate(context.Background(), "alice", "first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := limited.Generate(context.Background(), "alice", "second"); err == nil || inner.calls != 1 {
		t.Fatalf("image generation call limit was bypassed: calls=%d err=%v", inner.calls, err)
	}
}

func TestGenerateImageLoadsOrderedReferencesFromAttachmentStore(t *testing.T) {
	generator := &recordingImageGenerator{bytes: tinyPNG(t), mime: "image/png"}
	store := attachments.Store{API: attachments.LocalObjectAPI{Root: t.TempDir()}}
	var ids []string
	var originals [][]byte
	for _, size := range []int{2, 3} {
		var imageBytes bytes.Buffer
		if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, size, size))); err != nil {
			t.Fatal(err)
		}
		ref, err := store.SaveImage(t.Context(), imageBytes.Bytes(), "image/png", "reference.png", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ref.ID)
		originals = append(originals, imageBytes.Bytes())
	}
	tool := GenerateImage{Generator: generator, Attachments: store, References: store, UserID: "alice"}
	invalid, _ := json.Marshal(generateImageArgs{Prompt: "edit", ReferenceIDs: []string{ids[0], "sha256:" + strings.Repeat("0", 64)}})
	if _, _, err := tool.ExecuteContent(t.Context(), invalid); err == nil || generator.calls != 0 {
		t.Fatalf("missing reference reached provider: calls=%d err=%v", generator.calls, err)
	}
	valid, _ := json.Marshal(generateImageArgs{Prompt: "edit", ReferenceIDs: ids})
	_, blocks, err := tool.ExecuteContent(t.Context(), valid)
	if err != nil || len(blocks) != 1 || generator.calls != 1 || len(generator.refs) != len(ids) {
		t.Fatalf("edit returned %d blocks, %d calls and %d references: %v", len(blocks), generator.calls, len(generator.refs), err)
	}
	for i, reference := range generator.refs {
		if reference.MIMEType != "image/png" || !bytes.Equal(reference.Data, originals[i]) {
			t.Fatalf("image reference %d lost its bytes, MIME type or order", i)
		}
	}
}

func TestGenerateImageRejectsInvalidAttachment(t *testing.T) {
	generator := &recordingImageGenerator{bytes: []byte("invalid"), mime: "image/png"}
	store := attachments.Store{API: attachments.LocalObjectAPI{Root: t.TempDir()}}
	tool := GenerateImage{Generator: generator, Attachments: store, UserID: "alice"}
	text, blocks, err := tool.ExecuteContent(t.Context(), json.RawMessage(`{"prompt":"draw"}`))
	if err == nil || text != "" || len(blocks) != 0 || generator.calls != 1 {
		t.Fatalf("invalid image published a result: %q, %#v, calls=%d, err=%v", text, blocks, generator.calls, err)
	}
}
