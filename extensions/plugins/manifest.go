// Package plugins supplies reviewed first-party display metadata without loading JavaScript.
package plugins

import (
	"embed"
	"encoding/json"
	"fmt"
	"go-bot/plugin"
	"strings"
)

//go:embed */plugin.json */package.json */assets/*
var files embed.FS

// Metadata reads display copy from plugin.json and package identity from package.json.
// The catalog contains only the release selection and installation policy.
func Metadata(id string) (plugin.CatalogEntry, error) {
	var display struct {
		Name        string            `json:"name"`
		Description plugin.Localized  `json:"description"`
		Version     string            `json:"version"`
		Maintainer  string            `json:"maintainer"`
		License     string            `json:"license"`
		Icon        string            `json:"icon"`
		Tutorials   []plugin.Tutorial `json:"tutorials"`
	}
	raw, err := files.ReadFile(id + "/plugin.json")
	if err != nil {
		return plugin.CatalogEntry{}, err
	}
	if err = json.Unmarshal(raw, &display); err != nil {
		return plugin.CatalogEntry{}, err
	}
	raw, err = files.ReadFile(id + "/package.json")
	if err != nil {
		return plugin.CatalogEntry{}, err
	}
	var pkg struct{ Name, Version, License string }
	if err = json.Unmarshal(raw, &pkg); err != nil {
		return plugin.CatalogEntry{}, err
	}
	if display.Version != pkg.Version || display.License != pkg.License {
		return plugin.CatalogEntry{}, fmt.Errorf("%s: plugin.json version/license must match package.json", id)
	}
	if display.Name == "" {
		return plugin.CatalogEntry{}, fmt.Errorf("%s: plugin display name is missing", id)
	}
	if !strings.HasPrefix(display.Icon, AssetPrefix+id+"/") {
		return plugin.CatalogEntry{}, fmt.Errorf("%s: icon must reference the plugin's backend assets", id)
	}
	if _, err := Asset(display.Icon); err != nil {
		return plugin.CatalogEntry{}, fmt.Errorf("%s: plugin icon: %w", id, err)
	}
	languages := map[string]bool{}
	for _, tutorial := range display.Tutorials {
		if (tutorial.Language != "en" && tutorial.Language != "zh-CN") || languages[tutorial.Language] || strings.TrimSpace(tutorial.Title) == "" {
			return plugin.CatalogEntry{}, fmt.Errorf("%s: invalid tutorial language/title", id)
		}
		languages[tutorial.Language] = true
		for _, asset := range []string{tutorial.Video, tutorial.Poster} {
			if asset == "" && asset == tutorial.Poster {
				continue
			}
			if !strings.HasPrefix(asset, AssetPrefix+id+"/") {
				return plugin.CatalogEntry{}, fmt.Errorf("%s: tutorial must reference its backend assets", id)
			}
			if _, err := Asset(asset); err != nil {
				return plugin.CatalogEntry{}, fmt.Errorf("%s: tutorial asset: %w", id, err)
			}
		}
		if !strings.HasSuffix(tutorial.Video, ".mp4") {
			return plugin.CatalogEntry{}, fmt.Errorf("%s: tutorial video must be MP4", id)
		}
	}
	return plugin.CatalogEntry{Tutorials: display.Tutorials, ID: id, Package: pkg.Name, Version: display.Version, Name: display.Name, Description: display.Description, Icon: display.Icon, Maintainer: display.Maintainer, License: display.License}, nil
}
