package host

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go-harness/harness/session"
	"go-harness/internal/localstate"
)

func chatMetadataPath(store sessionformat.FileStore, id, name string) (string, error) {
	logPath, err := store.Path(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(logPath), name), nil
}

func archivedChat(store sessionformat.FileStore, id string) (bool, error) {
	marker, err := chatMetadataPath(store, id, "archived")
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func archiveChat(ctx context.Context, store sessionformat.FileStore, id string) error {
	if _, err := store.ReadHeader(ctx, id); err != nil {
		return err
	}
	marker, err := chatMetadataPath(store, id, "archived")
	if err != nil {
		return err
	}
	return localstate.WritePrivate(marker, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"))
}

func chatTitle(store sessionformat.FileStore, id string) string {
	path, err := chatMetadataPath(store, id, "title.txt")
	if err != nil {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 513))
	if err != nil || len(data) > 512 {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveChatTitle(store sessionformat.FileStore, id, title string) error {
	path, err := chatMetadataPath(store, id, "title.txt")
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return localstate.WritePrivate(path, []byte(title+"\n"))
}
