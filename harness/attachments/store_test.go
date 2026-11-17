package attachments

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"sync"
	"testing"

	"go-bot/internal/testsupport/session"
)

type memoryObjectAPI struct {
	mu           sync.Mutex
	objects      map[string][]byte
	putCalls     int
	lostResponse bool
}

func (a *memoryObjectAPI) PutIfAbsent(_ context.Context, key, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.putCalls++
	if a.objects == nil {
		a.objects = make(map[string][]byte)
	}
	if _, exists := a.objects[key]; !exists {
		a.objects[key] = append([]byte(nil), data...)
	}
	if a.lostResponse {
		a.lostResponse = false
		return errors.New("connection closed after upload")
	}
	return nil
}

func (a *memoryObjectAPI) Read(_ context.Context, key string) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	data, exists := a.objects[key]
	if !exists {
		return nil, errors.New("missing object")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func tinyImage(t *testing.T) []byte {
	t.Helper()
	imageValue := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	imageValue.Set(0, 0, color.NRGBA{R: 255, A: 255})
	imageValue.Set(1, 0, color.NRGBA{G: 255, A: 255})
	var output bytes.Buffer
	if err := png.Encode(&output, imageValue); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestImageStorePersistsBeforeReferenceAndVerifiesReads(t *testing.T) {
	ctx := context.Background()
	api := &memoryObjectAPI{lostResponse: true}
	store := Store{API: api, Prefix: "agents/"}
	source := tinyImage(t)
	ref, err := store.SaveImage(ctx, source, "image/png", "sample.png", "/workspace/sample.png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ref.ID, "sha256:") || ref.MIMEType != "image/png" || ref.Width != 2 || ref.Height != 1 || ref.ReadPath != "/workspace/sample.png" || ref.OriginalDimensions != nil {
		t.Fatalf("normalized reference = %#v", ref)
	}
	read, err := store.LoadImage(ctx, ref)
	if err != nil || !bytes.Equal(read, source) {
		t.Fatalf("stored bytes = %d, %v", len(read), err)
	}
	second, err := store.SaveImage(ctx, source, "image/png", "other-name.png", "/workspace/other-name.png")
	if err != nil || second.ID != ref.ID || len(api.objects) != 1 {
		t.Fatalf("same pixels were not deduplicated: %#v, objects=%d, %v", second, len(api.objects), err)
	}
	key, _ := store.key(ref.ID)
	api.mu.Lock()
	api.objects[key][0] ^= 0xff
	api.mu.Unlock()
	if _, err := store.LoadImage(ctx, ref); err == nil {
		t.Fatal("corrupted attachment bytes were accepted")
	}
	if _, err := store.SaveImage(ctx, source, "image/png", "again.png", ""); err == nil {
		t.Fatal("corrupted existing object was treated as a valid deduplication")
	}
}

// An offload placeholder points to the stored object. Reading that path again
// must produce a fresh message occurrence without producing another object ID.
func TestReadStoredNormalizedImageKeepsAttachmentIdentity(t *testing.T) {
	ctx := context.Background()
	api := &memoryObjectAPI{}
	store := Store{API: api}
	imageWithMetadata := pngWithTextChunk(t, tinyImage(t))
	first, err := store.SaveImage(ctx, imageWithMetadata, "image/png", "original.png", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.MIMEType != "image/jpeg" {
		t.Fatalf("metadata-bearing source was not normalized: %+v", first)
	}
	firstBytes, err := store.LoadImage(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveImage(ctx, firstBytes, first.MIMEType, "reread", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || len(api.objects) != 1 {
		t.Fatalf("rereading normalized image made a new object: first=%s second=%s count=%d", first.ID, second.ID, len(api.objects))
	}
	secondBytes, err := store.LoadImage(ctx, second)
	if err != nil || !bytes.Equal(secondBytes, firstBytes) {
		t.Fatalf("reread changed normalized bytes: %v", err)
	}
}

func TestMetadataFreeAlphaWebPPassesThrough(t *testing.T) {
	// A static VP8X/ALPH/VP8 image exercises the native alpha-image case.
	source, err := base64.StdEncoding.DecodeString("UklGRl4AAABXRUJQVlA4WAoAAAAQAAAAAQAAAAAAQUxQSAMAAAAAgIAAVlA4IDQAAADwAQCdASoCAAEAAQAcJaACdLoB+AAETAAA/vW4f/6aR40jxpHxcP/ugT90CfugT/3NoAAA")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{API: &memoryObjectAPI{}}
	ref, err := store.SaveImage(context.Background(), source, "image/webp", "alpha.webp", "")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.LoadImage(context.Background(), ref)
	if err != nil || ref.MIMEType != "image/webp" || !bytes.Equal(stored, source) {
		t.Fatalf("clean WebP was re-encoded: ref=%+v err=%v", ref, err)
	}
}

func pngWithTextChunk(t *testing.T, source []byte) []byte {
	t.Helper()
	const firstChunkEnd = 8 + 12 + 13
	if len(source) < firstChunkEnd {
		t.Fatal("test PNG has no IHDR")
	}
	payload := []byte("Comment\x00must be removed")
	kind := []byte("tEXt")
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], kind)
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	withText := append([]byte(nil), source[:firstChunkEnd]...)
	withText = append(withText, chunk...)
	return append(withText, source[firstChunkEnd:]...)
}

func TestImageStoreRejectsMismatchedTypeAndInvalidReference(t *testing.T) {
	store := Store{API: &memoryObjectAPI{}, Prefix: "agents/"}
	if _, err := store.SaveImage(context.Background(), tinyImage(t), "image/jpeg", "wrong.jpg", ""); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("declared MIME mismatch = %v", err)
	}
	if _, err := store.LoadImage(context.Background(), session.ImageAttachment{ID: "sha256:invalid"}); err == nil {
		t.Fatal("invalid reference was accepted")
	}
}

func TestImageRequestVariantKeepsCanonicalObjectAndTransparency(t *testing.T) {
	ctx := context.Background()
	store := Store{API: LocalObjectAPI{Root: t.TempDir()}}
	imageValue := image.NewNRGBA(image.Rect(0, 0, 8, 4))
	imageValue.SetNRGBA(0, 0, color.NRGBA{R: 128, A: 128})
	var source bytes.Buffer
	if err := png.Encode(&source, imageValue); err != nil {
		t.Fatal(err)
	}
	ref, err := store.SaveImage(ctx, source.Bytes(), "image/png", "alpha.png", "")
	if err != nil {
		t.Fatal(err)
	}
	requests := &RequestImages{Store: store, MaxWidth: 4, MaxHeight: 4, MaxBytes: 1024}
	variant, mediaType, err := requests.LoadRequestImage(ctx, ref)
	if err != nil || mediaType != "image/webp" || bytes.Equal(variant, source.Bytes()) {
		t.Fatalf("variant = %s, %d bytes, %v", mediaType, len(variant), err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(variant))
	if err != nil || decoded.Bounds().Dx() != 4 || decoded.Bounds().Dy() != 2 || !hasAlpha(decoded) {
		t.Fatalf("bad normalized variant: %v", err)
	}
	stored, err := store.LoadImage(ctx, ref)
	if err != nil || !bytes.Equal(stored, source.Bytes()) {
		t.Fatal("request variant changed the canonical image")
	}
}

func TestImageNormalizationAppliesEXIFOrientation(t *testing.T) {
	// Match the upstream orientation-6 fixture with a 4×2 JPEG.
	var input bytes.Buffer
	if err := jpeg.Encode(&input, image.NewRGBA(image.Rect(0, 0, 4, 2)), nil); err != nil {
		t.Fatal(err)
	}
	exif := []byte{
		'E', 'x', 'i', 'f', 0, 0,
		'I', 'I', 42, 0, 8, 0, 0, 0,
		1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0,
		0, 0, 0, 0,
	}
	source := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, byte(len(exif) + 2)}, exif...)
	source = append(source, input.Bytes()[2:]...)
	store := Store{API: LocalObjectAPI{Root: t.TempDir()}}
	ref, err := store.SaveImage(t.Context(), source, "image/jpeg", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Width != 2 || ref.Height != 4 || ref.MIMEType != "image/jpeg" || ref.OriginalDimensions != nil {
		t.Fatalf("rotated reference = %+v", ref)
	}
	data, err := store.LoadImage(t.Context(), ref)
	if err != nil || bytes.Contains(data, exif) || imageOrientation(data, ref.MIMEType) != 1 {
		t.Fatalf("normalized image retained EXIF metadata: %v", err)
	}
}

func TestImageNormalizationChecksSourceBudgetsAndDownscalesLongEdge(t *testing.T) {
	if _, err := normalizeImage(make([]byte, MaxImageSourceBytes+1), "image/png"); err == nil || !strings.Contains(err.Error(), "50 MiB") {
		t.Fatalf("encoded source size check = %v", err)
	}
	header := tinyImage(t)
	binary.BigEndian.PutUint32(header[16:20], 8001)
	binary.BigEndian.PutUint32(header[20:24], 8001)
	binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
	if _, err := normalizeImage(header, "image/png"); err == nil || !strings.Contains(err.Error(), "64-million-pixel") {
		t.Fatalf("source pixel check = %v", err)
	}
	var source bytes.Buffer
	longImage := image.NewGray(image.Rect(0, 0, maxNormalizedDimension+1, 1))
	if err := png.Encode(&source, longImage); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeImage(source.Bytes(), "image/png")
	if err != nil || got.Width != maxNormalizedDimension || got.Height != 1 || got.MIME != "image/jpeg" {
		t.Fatalf("long image normalized to %dx%d %s: %v", got.Width, got.Height, got.MIME, err)
	}
}

func TestImageEncodingKeepsSmallestJPEGWhenNoQualityFits(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for index := 0; index < len(source.Pix); index += 4 {
		pixel := index / 4
		source.Pix[index] = byte(pixel * 37)
		source.Pix[index+1] = byte(pixel * 17 >> 3)
		source.Pix[index+2] = byte(pixel ^ (pixel >> 3))
		source.Pix[index+3] = 255
	}
	smallest := 0
	for _, quality := range []int{85, 75, 60} {
		var candidate bytes.Buffer
		if err := jpeg.Encode(&candidate, source, &jpeg.Options{Quality: quality}); err != nil {
			t.Fatal(err)
		}
		if smallest == 0 || candidate.Len() < smallest {
			smallest = candidate.Len()
		}
	}
	data, mediaType, err := encodeNormalized(source, 512, false)
	if err != nil || mediaType != "image/jpeg" || len(data) != smallest || len(data) <= 512 {
		t.Fatalf("encoding fallback = %s, %d bytes, want %d: %v", mediaType, len(data), smallest, err)
	}
}

func TestImageRequestVariantMatchesOpaqueUpstreamFixture(t *testing.T) {
	// The upstream fixture requests 22×11 from a clean opaque 64×32 PNG.
	sourceImage := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			sourceImage.SetRGBA(x, y, color.RGBA{R: 12, G: 200, B: 64, A: 255})
		}
	}
	var source bytes.Buffer
	if err := png.Encode(&source, sourceImage); err != nil {
		t.Fatal(err)
	}
	store := Store{API: LocalObjectAPI{Root: t.TempDir()}}
	ref, err := store.SaveImage(t.Context(), source.Bytes(), "image/png", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		requests := &RequestImages{Store: store, MaxWidth: 22, MaxHeight: 11, MaxBytes: 4096}
		variant, mediaType, err := requests.LoadRequestImage(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		width, height, detected, err := inspectImage(variant)
		if err != nil || width != 22 || height != 11 || mediaType != "image/jpeg" || detected != mediaType || len(variant) > 4096 {
			t.Fatalf("variant = %dx%d %s, %d bytes, %v", width, height, mediaType, len(variant), err)
		}
		stored, err := store.LoadImage(t.Context(), ref)
		if err != nil || !bytes.Equal(stored, source.Bytes()) {
			t.Fatalf("canonical bytes changed: %v", err)
		}
	}
}
