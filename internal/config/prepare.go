package config

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go-bot/internal/localstate"
)

// PrepareControllerProfile is the launcher's deployment profile writer.
func PrepareControllerProfile(tuning, path string) error {
	path, err := privateProfilePath(path)
	if err != nil {
		return err
	}
	return prepareControllerProfile(tuning, path)
}

func privateProfilePath(path string) (string, error) {
	path = profilePath(path)
	if _, err := os.Lstat(path); err == nil {
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if filepath.Base(path) == ".env.example" {
		return "", errors.New("use a private environment profile instead of .env.example")
	}
	return path, nil
}

// SaveECIImageCacheID updates an existing private deployment profile.
func SaveECIImageCacheID(path, id string) error {
	if !ValidImageCacheID(id) {
		return errors.New("invalid ECI image cache ID")
	}
	path, err := privateProfilePath(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c := Default()
	if _, err := parseEnvironmentFile(path, string(data), controllerBindings(&c)); err != nil {
		return err
	}
	return localstate.WritePrivate(path, []byte(updateEnvironmentFile(string(data), map[string]string{"ECI_IMAGE_CACHE_ID": id})))
}

func prepareControllerProfile(tuning, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read deployment profile (initialize it with scripts/start.sh --init-env): %w", err)
	}
	c, err := loadTuning(tuning)
	if err != nil {
		return err
	}
	fields := controllerBindings(&c)
	values, err := parseEnvironmentFile(path, string(data), fields)
	if err != nil {
		return err
	}
	overrides := fields.processValues()
	for _, key := range []string{"MODEL_API_KEY", "MODEL_URL", "MODEL_NAME", "WEB_SEARCH_PROVIDER", "WEB_SEARCH_API_KEY"} {
		delete(overrides, key)
	}
	effective := maps.Clone(values)
	maps.Copy(effective, overrides)
	if err := fields.apply(effective); err != nil {
		return err
	}
	changes := make(map[string]string)
	for key, value := range overrides {
		if strings.ContainsAny(value, "\r\n\x00") || len(key)+len(value) >= bufio.MaxScanTokenSize-2 {
			return fmt.Errorf("%s cannot be saved as a single environment assignment", key)
		}
		if old, exists := values[key]; !exists || old != value {
			changes[key] = value
		}
	}
	if err := c.ValidateController(); err != nil {
		return fmt.Errorf("%w\nEdit %s or set the corresponding environment variables", err, path)
	}
	if len(changes) > 0 {
		return localstate.WritePrivate(path, []byte(updateEnvironmentFile(string(data), changes)))
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return os.Chmod(path, 0600)
	}
	return nil
}

func updateEnvironmentFile(data string, changes map[string]string) string {
	if len(changes) == 0 {
		return data
	}
	lines := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	for i, line := range lines {
		key, _, _ := strings.Cut(strings.TrimLeft(line, " \t"), "=")
		if value, ok := changes[key]; ok {
			lines[i] = key + "=" + value
			delete(changes, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		lines = append(lines, key+"="+changes[key])
	}
	return strings.Join(lines, "\n") + "\n"
}
