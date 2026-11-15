package attachments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"go-harness/harness/content"
)

const objectRoot = "attachments/v1/objects/"

var ErrInvalidImage = errors.New("invalid image attachment")

// ObjectAPI is deliberately smaller than the Session append API: an image is
// an immutable, content-addressed object, never a journal segment.
type ObjectAPI interface {
	PutIfAbsent(context.Context, string, string, []byte) error
	Read(context.Context, string) (io.ReadCloser, error)
}

// HostPathObjectAPI optionally exposes an immutable object in the Host's
// filesystem. The model adapter maps that path into its tool execution world.
type HostPathObjectAPI interface {
	HostPath(string) (string, error)
}

// Store is shared by a worker writer and the model capability reader. The
// object key is derived only from normalized image bytes, so the journal can
// persist a compact ID without embedding pixels or a temporary file path.
type Store struct {
	API    ObjectAPI
	Prefix string
}

func (s Store) validate() error {
	if s.API == nil || (s.Prefix != "" && (!strings.HasSuffix(s.Prefix, "/") || strings.HasPrefix(s.Prefix, "/") || strings.Contains(s.Prefix, "\\") || path.Clean(strings.TrimSuffix(s.Prefix, "/")) != strings.TrimSuffix(s.Prefix, "/") || strings.HasPrefix(s.Prefix, "../"))) {
		return errors.New("invalid attachment store or key prefix")
	}
	return nil
}

func (s Store) key(id string) (string, error) {
	if !strings.HasPrefix(id, "sha256:") || len(id) != len("sha256:")+64 {
		return "", errors.New("invalid image attachment ID")
	}
	digest := strings.TrimPrefix(id, "sha256:")
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		return "", errors.New("invalid image attachment digest")
	}
	return s.Prefix + objectRoot + digest[:2] + "/" + digest, nil
}

// ImageHostPath returns a provider-owned normalized object path when the
// attachment storage is visible to tools in this Host. It never uses ReadPath,
// which may point at a mutable or already deleted source file.
func (s Store) ImageHostPath(ref content.ImageAttachment) string {
	if err := s.validate(); err != nil {
		return ""
	}
	key, err := s.key(ref.ID)
	if err != nil {
		return ""
	}
	provider, ok := s.API.(HostPathObjectAPI)
	if !ok {
		return ""
	}
	path, err := provider.HostPath(key)
	if err != nil {
		return ""
	}
	return path
}

// SaveImage validates and normalizes before an immutable object write. It confirms that
// the published object can be read and matches its digest before returning a
// reference that may be committed in a tool/result event.
func (s Store) SaveImage(ctx context.Context, source []byte, declaredMIME, name, readPath string) (content.ImageAttachment, error) {
	if err := s.validate(); err != nil {
		return content.ImageAttachment{}, err
	}
	prepared, err := normalizeImage(source, declaredMIME)
	if err != nil {
		return content.ImageAttachment{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
	}
	digest := sha256.Sum256(prepared.Data)
	ref := content.ImageAttachment{
		ID: "sha256:" + hex.EncodeToString(digest[:]), MIMEType: prepared.MIME,
		Bytes: int64(len(prepared.Data)), Width: prepared.Width, Height: prepared.Height,
		Name: name, ReadPath: readPath,
	}
	if prepared.OriginalWidth != prepared.Width || prepared.OriginalHeight != prepared.Height {
		ref.OriginalDimensions = &content.ImageDimensions{Width: prepared.OriginalWidth, Height: prepared.OriginalHeight}
	}
	key, _ := s.key(ref.ID)
	if err := s.API.PutIfAbsent(ctx, key, prepared.MIME, prepared.Data); err != nil {
		// PutObject can succeed while its response is lost. A verified read is
		// still sufficient to admit the immutable reference.
		if _, readErr := s.LoadImage(ctx, ref); readErr != nil {
			return content.ImageAttachment{}, fmt.Errorf("save image attachment: %w; read-back: %v", err, readErr)
		}
		return ref, nil
	}
	if _, err := s.LoadImage(ctx, ref); err != nil {
		return content.ImageAttachment{}, fmt.Errorf("verify saved image attachment: %w", err)
	}
	return ref, nil
}

// LoadImage never falls back to ReadPath: a changed workspace file cannot
// silently replace pixels already referenced by durable conversation history.
func (s Store) LoadImage(ctx context.Context, ref content.ImageAttachment) ([]byte, error) {
	if ref.Bytes <= 0 || ref.Bytes > MaxImageSourceBytes || ref.Width <= 0 || ref.Height <= 0 || !supportedMIME(ref.MIMEType) {
		return nil, errors.New("invalid stored image attachment reference")
	}
	data, err := s.readImage(ctx, ref.ID, ref.Bytes)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Bytes {
		return nil, errors.New("stored image attachment length mismatch")
	}
	width, height, mime, err := inspectImage(data)
	if err != nil || width != ref.Width || height != ref.Height || mime != ref.MIMEType {
		return nil, errors.New("stored image attachment metadata mismatch")
	}
	return data, nil
}

// LoadReferenceImage resolves a content-addressed image for generation using
// the normalized bytes held by this Host's attachment store.
func (s Store) LoadReferenceImage(ctx context.Context, id string) (content.ImageInput, error) {
	data, err := s.readImage(ctx, id, maxNormalizedBytes)
	if err != nil {
		return content.ImageInput{}, err
	}
	_, _, mime, err := inspectImage(data)
	if err != nil {
		return content.ImageInput{}, err
	}
	return content.ImageInput{Data: data, MIMEType: mime}, nil
}

func (s Store) readImage(ctx context.Context, id string, limit int64) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	key, err := s.key(id)
	if err != nil {
		return nil, err
	}
	reader, err := s.API.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("stored image attachment exceeds its byte limit")
	}
	digest := sha256.Sum256(data)
	if id != "sha256:"+hex.EncodeToString(digest[:]) {
		return nil, errors.New("stored image attachment digest mismatch")
	}
	return data, nil
}
