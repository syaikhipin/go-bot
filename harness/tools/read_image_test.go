package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-bot/harness/attachments"
	"go-bot/internal/agentfs"
	"go-bot/internal/testsupport/session"
)

type recordingImageWriter struct {
	data                     []byte
	declaredMIME, name, path string
	ref                      session.ImageAttachment
}

func (w *recordingImageWriter) SaveImage(_ context.Context, data []byte, mime, name, path string) (session.ImageAttachment, error) {
	w.data = append([]byte(nil), data...)
	w.declaredMIME, w.name, w.path = mime, name, path
	return w.ref, nil
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestReadImageReturnsDurableImageBlock(t *testing.T) {
	root := t.TempDir()
	data := testPNG(t)
	if err := os.WriteFile(filepath.Join(root, "sample.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	writer := &recordingImageWriter{ref: session.ImageAttachment{ID: "sha256:sample", MIMEType: "image/png", Width: 2, Height: 2, Bytes: int64(len(data)), ReadPath: "sample.png"}}
	tool := ReadImage{View: agentfs.ContainerView{DefaultWorkdir: root}, Attachments: writer, ImageInputEnabled: true}
	text, blocks, err := tool.ExecuteContent(context.Background(), json.RawMessage(`{"file_path":"sample.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "sample.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(writer.data, data) || writer.declaredMIME != "image/png" || writer.name != "sample.png" || writer.path != resolved {
		t.Fatalf("image was not passed to durable writer: %#v", writer)
	}
	if len(blocks) != 1 || blocks[0].Type != "image" || blocks[0].ImageRefID != writer.ref.ID || !strings.Contains(text, "<path>"+resolved+"</path>") {
		t.Fatalf("result does not contain image reference: text=%q blocks=%#v", text, blocks)
	}
}

func TestReadImageCanReopenOffloadedNormalizedObject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := attachments.Store{API: attachments.LocalObjectAPI{Root: filepath.Join(root, ".harness")}}
	ref, err := store.SaveImage(ctx, testPNG(t), "image/png", "source.png", filepath.Join(root, "source.png"))
	if err != nil {
		t.Fatal(err)
	}
	path := store.ImageHostPath(ref)
	if path == "" {
		t.Fatal("offloaded image has no normalized object path")
	}
	args, _ := json.Marshal(map[string]string{"file_path": path})
	tool := ReadImage{View: agentfs.ContainerView{DefaultWorkdir: root}, Attachments: store, ImageInputEnabled: true}
	_, blocks, err := tool.ExecuteContent(ctx, args)
	if err != nil || len(blocks) != 1 || blocks[0].ImageRefID != ref.ID {
		t.Fatalf("readimage normalized copy = %#v, %v", blocks, err)
	}
}

func TestReadImageReportsOpenedSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "original.png"), testPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("original.png", filepath.Join(root, "alias.png")); err != nil {
		t.Fatal(err)
	}
	writer := &recordingImageWriter{ref: session.ImageAttachment{ID: "sha256:sample", MIMEType: "image/png", Width: 2, Height: 2, Bytes: 12}}
	tool := ReadImage{View: agentfs.ContainerView{DefaultWorkdir: root}, Attachments: writer, ImageInputEnabled: true}
	text, _, err := tool.ExecuteContent(context.Background(), json.RawMessage(`{"file_path":"alias.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, "original.png"))
	if err != nil {
		t.Fatal(err)
	}
	if writer.path != resolved || writer.name != "original.png" || !strings.Contains(text, "<path>"+resolved+"</path>") {
		t.Fatalf("image did not report the opened target: path=%q name=%q text=%q", writer.path, writer.name, text)
	}
}

func TestReadImageGatesBeforeFileAccess(t *testing.T) {
	w := &recordingImageWriter{}
	tool := ReadImage{View: agentfs.ContainerView{DefaultWorkdir: t.TempDir()}, Attachments: w, ImageInputEnabled: false}
	_, _, err := tool.ExecuteContent(context.Background(), json.RawMessage(`{"file_path":"does-not-exist.png"}`))
	if err == nil || !strings.Contains(err.Error(), "image input") {
		t.Fatalf("route gate did not run first: %v", err)
	}
	tool.ImageInputEnabled = true
	tool.View = agentfs.ContainerView{DefaultWorkdir: t.TempDir()}
	_, _, err = tool.ExecuteContent(context.Background(), json.RawMessage(`{"file_path":"unsupported.svg"}`))
	if err == nil || !strings.Contains(err.Error(), "extension") || len(w.data) != 0 {
		t.Fatalf("unsupported extension reached file read or writer: %v", err)
	}
	_, _, err = tool.ExecuteContent(context.Background(), json.RawMessage(`{"file_path":"missing.png"}`))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing image = %v", err)
	}
}

func TestReadImageReportsOriginalDimensionsAndCoordinateScale(t *testing.T) {
	for _, test := range []struct {
		name string
		ref  session.ImageAttachment
		want string
	}{
		{
			name: "same scale", ref: session.ImageAttachment{MIMEType: "image/png", Width: 200, Height: 100, Bytes: 123,
				OriginalDimensions: &session.ImageDimensions{Width: 800, Height: 400}},
			want: "downscaled from 800x400 px; multiply coordinates by 4.00 to locate features in the original file",
		},
		{
			name: "different rounded scales", ref: session.ImageAttachment{MIMEType: "image/png", Width: 200, Height: 99, Bytes: 123,
				OriginalDimensions: &session.ImageDimensions{Width: 800, Height: 400}},
			want: "multiply x coordinates by 4.00 and y coordinates by 4.04",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := formatImageReadOutput("/root/harness/workspaces/example/image.png", test.ref)
			if !strings.Contains(got, test.want) {
				t.Fatalf("image output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReadImageRejectsTrailingJSON(t *testing.T) {
	if _, err := decodeReadImageArgs(json.RawMessage(`{"file_path":"sample.png"}{"file_path":"other.png"}`)); err == nil {
		t.Fatal("readimage accepted a second JSON object")
	}
}
