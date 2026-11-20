// Package catalog embeds the project's reviewed business plugin releases.
// It is build-time data, never a user-configured remote index.
package catalog

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"regexp"
	"strings"

	bundled "go-bot/extensions/plugins"
	"go-bot/plugin"
)

//go:embed plugins.json
var manifest []byte

type Entry struct {
	plugin.CatalogEntry
	Source plugin.Source `json:"source"`
	Builds []string      `json:"builds,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// Entries returns a fresh copy so request processing cannot mutate the catalog.
func Entries() []Entry {
	entries, err := Parse(manifest)
	if err != nil {
		panic("invalid bundled plugin catalog: " + err.Error())
	}
	return entries
}

func Parse(data []byte) ([]Entry, error) {
	var entries []Entry
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&entries); err != nil {
		return nil, err
	}
	if !errors.Is(d.Decode(new(any)), io.EOF) || entries == nil || len(entries) > 200 {
		return nil, errors.New("catalog must be one array of at most 200 releases")
	}
	ids, packages := map[string]bool{}, map[string]bool{}
	for i := range entries {
		entry := &entries[i]
		if entry.Source.Type == "bundled" {
			display, err := bundled.Metadata(entry.Source.Spec)
			if err != nil {
				return nil, err
			}
			if display.ID != entry.ID || display.Package != entry.Package || display.Version != entry.Version {
				return nil, fmt.Errorf("%s: bundled identity differs from the catalog", entry.ID)
			}
			entry.CatalogEntry = display
		}
		if !idPattern.MatchString(entry.ID) || ids[entry.ID] || packages[entry.Package] {
			return nil, errors.New("catalog plugin IDs and package names must be unique")
		}
		ids[entry.ID], packages[entry.Package] = true, true
		if !versionPattern.MatchString(entry.Version) || entry.Maintainer == "" || entry.License == "" || strings.TrimSpace(entry.Name) == "" {
			return nil, fmt.Errorf("%s: version, maintainer and license are required", entry.ID)
		}
		for _, text := range []plugin.Localized{entry.Description} {
			if strings.TrimSpace(text.EN) == "" || strings.TrimSpace(text.ZH) == "" {
				return nil, fmt.Errorf("%s: English and Simplified Chinese copy are required", entry.ID)
			}
		}
		if !strings.HasPrefix(entry.Icon, bundled.AssetPrefix) || !fs.ValidPath(strings.TrimPrefix(entry.Icon, bundled.AssetPrefix)) || strings.ContainsAny(entry.Icon, "\\?#") {
			return nil, fmt.Errorf("%s: icon must reference a backend plugin asset", entry.ID)
		}

		settings := plugin.RuntimeSettings{Packages: map[string]plugin.RuntimePackage{entry.Package: {Source: entry.Source, Builds: entry.Builds}}}
		if err := settings.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.ID, err)
		}
		switch entry.Source.Type {
		case "registry":
			if entry.Source.Spec != entry.Package+"@"+entry.Version {
				return nil, fmt.Errorf("%s: registry source must match the catalog release", entry.ID)
			}
		case "bundled":
			if entry.Source.Spec != entry.ID {
				return nil, fmt.Errorf("%s: bundled source must match plugin ID", entry.ID)
			}
		case "git": // Settings.Validate requires a full commit, never a branch.
		case "tarball":
			u, err := url.Parse(entry.Source.Spec)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, fmt.Errorf("%s: distribution archives require a fixed HTTPS release URL", entry.ID)
			}
		default:
			return nil, fmt.Errorf("%s: development directories are not market releases", entry.ID)
		}
	}
	return entries, nil
}
