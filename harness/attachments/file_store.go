package attachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"go-harness/internal/durablefs"
)

const maxUploadedFileBytes = 32 << 20

var fileDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var windowsDeviceStem = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)

// FileRef is the native DeepSeek file block reference. It contains no path:
// the Host resolves it in its immutable attachment store.
type FileRef struct {
	ID    string `json:"attachmentId"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// FileStore keeps uploaded bytes verbatim under the Host DSH home, including when that home is on NAS.
type FileStore struct{ Root string }

func fileLeafName(value string) string {
	if slash := strings.LastIndexAny(value, `/\`); slash >= 0 {
		value = value[slash+1:]
	}
	var out strings.Builder
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			continue
		}
		if strings.ContainsRune(`<>:"|?*`, char) {
			char = '_'
		}
		if out.Len()+utf8.RuneLen(char) > 255 {
			break
		}
		out.WriteRune(char)
	}
	name := strings.TrimSpace(out.String())
	name = strings.TrimRight(name, ". ")
	stem := name
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	if windowsDeviceStem.MatchString(stem) {
		name = "_" + name
	}
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// SafeFileName strips client path components and unsafe display characters.
func SafeFileName(value string) string { return fileLeafName(value) }

func (s FileStore) path(ref FileRef) (string, error) {
	if !filepath.IsAbs(s.Root) || !fileDigestPattern.MatchString(ref.ID) || ref.Name != fileLeafName(ref.Name) || ref.Bytes < 0 {
		return "", errors.New("invalid native file attachment reference")
	}
	digest := strings.TrimPrefix(ref.ID, "sha256:")
	return filepath.Join(s.Root, "attachments", "v1", "files", digest[:2], digest, ref.Name), nil
}

func (s FileStore) objectPath(ref FileRef) (string, error) {
	if _, err := s.path(ref); err != nil {
		return "", err
	}
	digest := strings.TrimPrefix(ref.ID, "sha256:")
	return filepath.Join(s.Root, "attachments", "v1", "file-objects", digest[:2], digest), nil
}

// Path resolves a durable reference only if the saved copy remains available.
// The Agent may then read it with ordinary tools; the copy is not a policy
// boundary against a root container modifying its own workspace.
func (s FileStore) Path(ref FileRef) string {
	path, err := s.path(ref)
	if err != nil {
		return ""
	}
	if err := verifyFilePath(path, ref); err != nil {
		return ""
	}
	return path
}

// SaveFile commits exact uploaded bytes and verifies their persisted copy
// before its reference can enter the native Session inbox.
func (s FileStore) SaveFile(ctx context.Context, input io.Reader, originalName string) (FileRef, error) {
	if !filepath.IsAbs(s.Root) || input == nil {
		return FileRef{}, errors.New("invalid native file attachment store")
	}
	if err := ctx.Err(); err != nil {
		return FileRef{}, err
	}
	data, err := io.ReadAll(io.LimitReader(input, maxUploadedFileBytes+1))
	if err != nil {
		return FileRef{}, err
	}
	if len(data) > maxUploadedFileBytes {
		return FileRef{}, errors.New("native file attachment exceeds 32 MiB")
	}
	digest := sha256.Sum256(data)
	ref := FileRef{ID: "sha256:" + hex.EncodeToString(digest[:]), Name: fileLeafName(originalName), Bytes: int64(len(data))}
	object, err := s.objectPath(ref)
	if err != nil {
		return FileRef{}, err
	}
	if err := publishFileObject(ctx, object, data, ref); err != nil {
		return FileRef{}, err
	}
	alias, err := s.path(ref)
	if err != nil {
		return FileRef{}, err
	}
	if err := durablefs.EnsureDirectory(filepath.Dir(alias)); err != nil {
		return FileRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return FileRef{}, err
	}
	if err := os.Link(object, alias); err != nil && !errors.Is(err, os.ErrExist) {
		return FileRef{}, err
	}
	if err := syncFileParent(alias); err != nil {
		return FileRef{}, err
	}
	if err := s.verify(ref); err != nil {
		return FileRef{}, err
	}
	return ref, nil
}

// One canonical object backs every display-name alias for the same bytes.
// Both publications use no-replace hard links on the Host data filesystem.
func publishFileObject(ctx context.Context, path string, data []byte, ref FileRef) error {
	if err := durablefs.EnsureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := verifyFilePath(path, ref); err == nil {
		return syncFileParent(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".attachment-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	modeErr := file.Chmod(0o400)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, modeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := syncFileParent(path); err != nil {
		return err
	}
	return verifyFilePath(path, ref)
}

func syncFileParent(path string) error {
	return durablefs.SyncDirectory(filepath.Dir(path))
}

func (s FileStore) verify(ref FileRef) error {
	path, err := s.path(ref)
	if err != nil {
		return err
	}
	return verifyFilePath(path, ref)
}

// ReadFile verifies the same immutable bytes it returns to the caller.
func (s FileStore) ReadFile(ctx context.Context, ref FileRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(ref)
	if err != nil {
		return nil, err
	}
	var data bytes.Buffer
	if err := readVerifiedFile(path, ref, &data); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func verifyFilePath(path string, ref FileRef) error {
	return readVerifiedFile(path, ref, io.Discard)
}

func readVerifiedFile(path string, ref FileRef, output io.Writer) error {
	file, err := openAttachment(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	read, readErr := io.Copy(io.MultiWriter(hash, output), io.LimitReader(file, maxUploadedFileBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if read != ref.Bytes || ref.ID != "sha256:"+hex.EncodeToString(hash.Sum(nil)) {
		return fmt.Errorf("native file attachment %s failed read-back verification", ref.ID)
	}
	return nil
}

func openAttachment(filename string) (*os.File, error) {
	file, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("attachment object is not a regular file")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
