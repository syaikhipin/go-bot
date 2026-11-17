package skills

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const (
	UserDSHHomeDir = ".dsh"
	UserDSHHome    = "/root/harness/" + UserDSHHomeDir
	UserAgentsHome = "/root/harness/.agents"
	SeedDir        = "/usr/share/go-bot/skill-seed"
	maxSkillBytes  = 256 << 10
)

var (
	skillName      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	errOutsideRoot = errors.New("skill path escapes its root")
	errNotRegular  = errors.New("skill entry is not a regular file")
)

func IsName(name string) bool { return skillName.MatchString(name) }

// IsSpace implements ECMAScript whitespace for catalog descriptions and user gestures.
func IsSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' ||
		r == '\u2028' || r == '\u2029' || r == '\ufeff' || unicode.Is(unicode.Zs, r)
}

type Lookup struct {
	CWD       string
	Workspace string
}

// Registry discovers filesystem skills in precedence order. Custom and bundled
// roots are trusted extensions supplied by the host embedding this package.
type Registry struct {
	DSHHome    string
	AgentsHome string
	CustomDirs []string
}

type Summary struct {
	Name           string
	Description    string
	WhenToUse      string
	Metadata       map[string]any
	ModelInvocable bool
	UserInvocable  bool
}

type ResourceBase struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type Value struct {
	Name         string        `json:"name"`
	Provider     string        `json:"provider"`
	ResourceBase *ResourceBase `json:"resourceBase,omitempty"`
	Content      string        `json:"content"`
}

type Loaded struct {
	Summary
	Value Value
}

type Candidate struct {
	Summary
	source skillRoot
	root   string
	path   string
	base   string
}

type Snapshot struct {
	Entries  []Candidate
	Complete bool
}

type skillRoot struct {
	path       string
	boundary   string
	skipSystem bool
}

func (r *Registry) roots(lookup Lookup) []skillRoot {
	var roots []skillRoot
	if project, workspace := projectRoot(lookup); project != "" {
		roots = append(roots,
			skillRoot{path: filepath.Join(project, ".dsh", "skills"), boundary: workspace},
			skillRoot{path: filepath.Join(project, ".agents", "skills"), boundary: workspace},
		)
	}
	for _, path := range r.CustomDirs {
		roots = append(roots, skillRoot{path: path})
	}
	if r.DSHHome != "" {
		roots = append(roots, skillRoot{path: filepath.Join(r.DSHHome, "skills"), boundary: r.DSHHome, skipSystem: true})
	}
	if r.AgentsHome != "" {
		roots = append(roots, skillRoot{path: filepath.Join(r.AgentsHome, "skills"), boundary: r.AgentsHome})
	}
	return roots
}

func projectRoot(lookup Lookup) (string, string) {
	if !filepath.IsAbs(lookup.CWD) || !filepath.IsAbs(lookup.Workspace) {
		return "", ""
	}
	workspace, err := filepath.EvalSymlinks(lookup.Workspace)
	if err != nil {
		return "", ""
	}
	cwd, err := filepath.EvalSymlinks(lookup.CWD)
	if err != nil || !within(workspace, cwd) {
		return "", ""
	}
	for current := cwd; ; current = filepath.Dir(current) {
		if info, err := os.Stat(filepath.Join(current, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return current, workspace
		}
		if current == workspace {
			return cwd, workspace
		}
	}
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (r *Registry) Snapshot(ctx context.Context, lookup Lookup) (Snapshot, error) {
	snapshot := Snapshot{Complete: true}
	winners := make(map[string]Candidate)
	for _, source := range r.roots(lookup) {
		entries, err := discover(ctx, source)
		if ctx.Err() != nil {
			return Snapshot{}, ctx.Err()
		}
		if err != nil {
			snapshot.Complete = false
			slog.Warn("skill root unavailable", "path", source.path, "error", err)
			continue
		}
		for _, entry := range entries {
			if _, exists := winners[entry.Name]; !exists {
				winners[entry.Name] = entry
			}
		}
	}
	for _, entry := range winners {
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	slices.SortFunc(snapshot.Entries, func(a, b Candidate) int { return strings.Compare(a.Name, b.Name) })
	return snapshot, ctx.Err()
}

func (r *Registry) Find(ctx context.Context, lookup Lookup, name string) (Candidate, bool, error) {
	if !IsName(name) {
		return Candidate{}, false, nil
	}
	snapshot, err := r.Snapshot(ctx, lookup)
	if err != nil {
		return Candidate{}, false, err
	}
	index, found := slices.BinarySearchFunc(snapshot.Entries, name, func(entry Candidate, name string) int {
		return strings.Compare(entry.Name, name)
	})
	if !found {
		return Candidate{}, false, nil
	}
	return snapshot.Entries[index], true, nil
}

// Load revalidates the selected locator and identity against current disk content.
func (c Candidate) Load(ctx context.Context) (Loaded, bool, error) {
	if err := ctx.Err(); err != nil {
		return Loaded{}, false, err
	}
	root, err := openSkillRoot(c.source)
	if err != nil {
		return Loaded{}, false, ctx.Err()
	}
	defer root.Close()
	if root.Name() != c.root {
		return Loaded{}, false, ctx.Err()
	}
	loaded, err := load(root, c.path, c.base)
	if err != nil {
		slog.Debug("skill no longer readable", "path", filepath.Join(c.root, c.path), "error", err)
	}
	return loaded, err == nil && loaded.Name == c.Name, ctx.Err()
}

func openSkillRoot(source skillRoot) (*os.Root, error) {
	path, err := filepath.EvalSymlinks(source.path)
	if err != nil {
		return nil, err
	}
	if source.boundary == "" {
		return os.OpenRoot(path)
	}
	info, err := os.Lstat(source.boundary)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errOutsideRoot
	}
	anchor, err := filepath.EvalSymlinks(source.boundary)
	if err != nil {
		return nil, err
	}
	if !within(anchor, path) {
		return nil, errOutsideRoot
	}
	if anchor == path {
		return os.OpenRoot(path)
	}
	boundary, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	defer boundary.Close()
	relative, err := filepath.Rel(anchor, path)
	if err != nil {
		return nil, err
	}
	return boundary.OpenRoot(relative)
}

func discover(ctx context.Context, source skillRoot) ([]Candidate, error) {
	root, err := openSkillRoot(source)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	order := collate.New(language.English)
	slices.SortStableFunc(entries, func(a, b os.DirEntry) int { return order.CompareString(a.Name(), b.Name()) })
	var candidates []Candidate
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if source.skipSystem && name == ".system" {
			continue
		}
		resolved, err := resolve(root, name)
		if absent(err) || errors.Is(err, errOutsideRoot) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := root.Stat(resolved)
		if absent(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		locator, base := name, "."
		switch {
		case info.IsDir():
			locator, base = filepath.Join(name, "SKILL.md"), name
		case info.Mode().IsRegular() && strings.HasSuffix(name, ".md"):
		default:
			continue
		}
		loaded, err := load(root, locator, base)
		if absent(err) || errors.Is(err, errOutsideRoot) || errors.Is(err, errNotRegular) {
			continue
		}
		var invalid invalidSkill
		if errors.As(err, &invalid) {
			slog.Warn("ignoring invalid skill", "path", filepath.Join(source.path, locator), "reason", invalid.Error())
			continue
		}
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, Candidate{Summary: loaded.Summary, source: source, root: root.Name(), path: locator, base: base})
	}
	return candidates, nil
}

func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func resolve(root *os.Root, path string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(root.Name(), path))
	if err != nil {
		return "", err
	}
	if !within(root.Name(), real) {
		return "", errOutsideRoot
	}
	return filepath.Rel(root.Name(), real)
}

func load(root *os.Root, locator, base string) (Loaded, error) {
	path, err := resolve(root, locator)
	if err != nil {
		return Loaded{}, err
	}
	info, err := root.Stat(path)
	if err != nil {
		return Loaded{}, err
	}
	if !info.Mode().IsRegular() {
		return Loaded{}, errNotRegular
	}
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Loaded{}, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return Loaded{}, err
	}
	if !info.Mode().IsRegular() {
		return Loaded{}, errNotRegular
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSkillBytes+1))
	if err != nil {
		return Loaded{}, err
	}
	if len(data) > maxSkillBytes {
		return Loaded{}, invalidSkill("entry exceeds the skill size limit")
	}
	summary, body, err := parseSkill(string(data))
	if err != nil {
		return Loaded{}, err
	}
	return Loaded{Summary: summary, Value: Value{
		Name: summary.Name, Provider: "filesystem", Content: body,
		ResourceBase: &ResourceBase{Kind: "directory", Path: filepath.Join(root.Name(), base)},
	}}, nil
}
