package assembly

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"go-harness/harness/agent"
	"go-harness/internal/instructionrender"
)

// Match the standard DeepSeek preset's rendered budget and source-read cap.
const nativeInstructionMaxBytes = 64 << 10
const nativeInstructionMaxSourceBytes = 1 << 20

var nativeInstructionCandidates = []string{"AGENTS.md", "CLAUDE.md", "AGENTS.local.md", "CLAUDE.local.md"}

type nativeInstructionFile struct {
	path    string
	group   string
	display string
	content string
}

type nativeInstructionReader struct {
	cache map[string]nativeInstructionCachedFile
	reads int // actual content reads, useful for verifying the metadata fast path
}

type nativeInstructionCachedFile struct {
	info    os.FileInfo
	content string
}

type nativeInstructionProbe struct {
	status  string // present, absent, or unavailable
	content string
}

func loadNativeWorkspaceInstructions(ctx context.Context, cwd string, touched ...string) (agent.NativeInstructionSnapshot, error) {
	return (&nativeInstructionReader{}).loadCurrent(ctx, cwd, touched...)
}

func (reader *nativeInstructionReader) loadCurrent(ctx context.Context, cwd string, touched ...string) (agent.NativeInstructionSnapshot, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return agent.NativeInstructionSnapshot{}, err
	}
	dshHome := strings.TrimSpace(os.Getenv("DSH_HOME"))
	if dshHome == "" {
		dshHome = filepath.Join(home, ".dsh")
	}
	agentsHome := strings.TrimSpace(os.Getenv("DSH_AGENTS_HOME"))
	if agentsHome == "" {
		agentsHome = filepath.Join(home, ".agents")
	}
	return reader.loadFrom(ctx, cwd, dshHome, agentsHome, touched...)
}

func loadNativeWorkspaceInstructionsFrom(ctx context.Context, cwd, dshHome, agentsHome string, touched ...string) (agent.NativeInstructionSnapshot, error) {
	return (&nativeInstructionReader{}).loadFrom(ctx, cwd, dshHome, agentsHome, touched...)
}

func (reader *nativeInstructionReader) loadFrom(ctx context.Context, cwd, dshHome, agentsHome string, touched ...string) (agent.NativeInstructionSnapshot, error) {
	if !filepath.IsAbs(cwd) {
		return agent.NativeInstructionSnapshot{}, errors.New("workspace instruction cwd must be absolute")
	}
	cwd = filepath.Clean(cwd)
	root, err := nativeInstructionProjectRoot(ctx, cwd)
	if err != nil {
		return agent.NativeInstructionSnapshot{}, err
	}
	relativeRoot, err := filepath.Rel(cwd, root)
	if err != nil {
		return agent.NativeInstructionSnapshot{}, err
	}
	identityBytes, err := json.Marshal(struct {
		Root            string   `json:"projectRoot"`
		Markers         []string `json:"projectRootMarkers"`
		MaxBytes        int      `json:"maxBytes"`
		MaxSourceBytes  int      `json:"maxSourceBytes"`
		Candidates      []string `json:"instructionFileCandidates"`
		LocalCandidates []string `json:"localInstructionFileCandidates"`
	}{relativeRoot, []string{".git"}, nativeInstructionMaxBytes, nativeInstructionMaxSourceBytes,
		nativeInstructionCandidates[:2], nativeInstructionCandidates[2:]})
	if err != nil {
		return agent.NativeInstructionSnapshot{}, err
	}
	snapshot := agent.NativeInstructionSnapshot{Identity: string(identityBytes), UnavailableGroups: make(map[string]bool)}
	var files []nativeInstructionFile
	groups := map[string]map[[32]byte]bool{}
	add := func(group, path, display string) error {
		probe, err := reader.read(ctx, path)
		if err != nil {
			return err
		}
		if probe.status == "unavailable" {
			snapshot.UnavailableGroups[group] = true
			return nil
		}
		if probe.status != "present" {
			return nil
		}
		digest := sha256.Sum256([]byte(strings.TrimSpace(probe.content)))
		if groups[group] == nil {
			groups[group] = make(map[[32]byte]bool)
		}
		if groups[group][digest] {
			return nil
		}
		groups[group][digest] = true
		files = append(files, nativeInstructionFile{path: path, group: group, display: display, content: probe.content})
		return nil
	}
	home, _ := os.UserHomeDir()
	dshDisplay := "~/.dsh/AGENTS.md"
	if filepath.Clean(dshHome) != filepath.Join(home, ".dsh") {
		dshDisplay = "$DSH_HOME/AGENTS.md"
	}
	agentsDisplay := "~/.agents/AGENTS.md"
	if filepath.Clean(agentsHome) != filepath.Join(home, ".agents") {
		agentsDisplay = "$DSH_AGENTS_HOME/AGENTS.md"
	}
	if err := add("user-global", filepath.Join(dshHome, "AGENTS.md"), dshDisplay); err != nil {
		return snapshot, err
	}
	if err := add("user-global", filepath.Join(agentsHome, "AGENTS.md"), agentsDisplay); err != nil {
		return snapshot, err
	}
	var dirs []string
	for dir := cwd; ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == root {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		group, err := nativeInstructionProjectRelative(root, dir)
		if err != nil {
			return snapshot, err
		}
		for _, candidate := range nativeInstructionCandidates {
			path := filepath.Join(dir, candidate)
			display, err := nativeInstructionProjectRelative(root, path)
			if err != nil {
				return snapshot, err
			}
			if err := add(group, path, display); err != nil {
				return snapshot, err
			}
		}
	}
	visited := make(map[string]bool)
	for _, dir := range dirs {
		visited[dir] = true
	}
	for _, touchedPath := range touched {
		path := touchedPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		parent := filepath.Dir(path)
		rel, err := filepath.Rel(cwd, parent)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		var nested []string
		for dir := parent; dir != cwd; dir = filepath.Dir(dir) {
			if !visited[dir] {
				nested = append(nested, dir)
				visited[dir] = true
			}
		}
		for i := len(nested) - 1; i >= 0; i-- {
			group, err := nativeInstructionProjectRelative(root, nested[i])
			if err != nil {
				return snapshot, err
			}
			for _, candidate := range nativeInstructionCandidates {
				path := filepath.Join(nested[i], candidate)
				display, err := nativeInstructionProjectRelative(root, path)
				if err != nil {
					return snapshot, err
				}
				if err := add(group, path, display); err != nil {
					return snapshot, err
				}
			}
		}
	}
	addChanges := func(included []nativeInstructionFile) []agent.NativeInstructionChange {
		changes := make([]agent.NativeInstructionChange, 0, len(included))
		for _, file := range included {
			digest := sha1.Sum([]byte(file.content))
			changes = append(changes, agent.NativeInstructionChange{
				Action: "set", Scope: nativeInstructionFileScope(file), Path: file.display, Digest: hex.EncodeToString(digest[:]),
			})
		}
		return changes
	}
	replacementText, filesForReplacement := renderNativeInstructionBaseline(files, true)
	snapshot.ReplacementText = replacementText
	snapshot.ReplacementChanges = addChanges(filesForReplacement)
	if len(files) > 0 {
		var included []nativeInstructionFile
		snapshot.Text, included = renderNativeInstructionBaseline(files, false)
		snapshot.Changes = addChanges(included)
		snapshot.Content = make(map[string]string, len(included))
		for _, file := range included {
			snapshot.Content[nativeInstructionFileScope(file)] = file.content
		}
	}
	return snapshot, nil
}

func nativeInstructionFileScope(file nativeInstructionFile) string {
	switch file.display {
	case "~/.dsh/AGENTS.md", "$DSH_HOME/AGENTS.md":
		return "user-global\x00AGENTS.md"
	case "~/.agents/AGENTS.md", "$DSH_AGENTS_HOME/AGENTS.md":
		return "agents-global\x00AGENTS.md"
	default:
		return file.group + "\x00" + filepath.Base(file.display)
	}
}

// Reserve upstream's user-global and agents-global scope namespaces even
// when a project happens to use either name as its first directory component.
func nativeInstructionProjectRelative(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return rel, err
	}
	first := rel
	if index := strings.IndexRune(rel, filepath.Separator); index >= 0 {
		first = rel[:index]
	}
	if first == "user-global" || first == "agents-global" {
		return "." + string(filepath.Separator) + rel, nil
	}
	return rel, nil
}

func nativeInstructionProjectRoot(ctx context.Context, cwd string) (string, error) {
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			return dir, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return "", err
		}
		if parent := filepath.Dir(dir); parent == dir {
			return cwd, nil
		}
	}
}

// An absent file and an unavailable file are different: only confirmed absence
// can remove a previously visible instruction. Cached metadata avoids reading
// unchanged prose on every model step, mirroring upstream's version fast path.
func (reader *nativeInstructionReader) read(ctx context.Context, path string) (nativeInstructionProbe, error) {
	if err := ctx.Err(); err != nil {
		return nativeInstructionProbe{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			delete(reader.cache, path)
			return nativeInstructionProbe{status: "absent"}, nil
		}
		return nativeInstructionProbe{status: "unavailable"}, nil
	}
	if !info.Mode().IsRegular() {
		delete(reader.cache, path)
		return nativeInstructionProbe{status: "absent"}, nil
	}
	if info.Size() > nativeInstructionMaxSourceBytes {
		return nativeInstructionProbe{status: "unavailable"}, nil
	}
	if cached, ok := reader.cache[path]; ok && cached.info.Size() == info.Size() &&
		cached.info.ModTime().Equal(info.ModTime()) && os.SameFile(cached.info, info) {
		return nativeInstructionProbe{status: "present", content: cached.content}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nativeInstructionProbe{status: "unavailable"}, nil
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) ||
		opened.Size() != info.Size() || !opened.ModTime().Equal(info.ModTime()) {
		return nativeInstructionProbe{status: "unavailable"}, nil
	}
	bytes, err := io.ReadAll(io.LimitReader(file, nativeInstructionMaxSourceBytes+1))
	if err != nil || len(bytes) > nativeInstructionMaxSourceBytes {
		return nativeInstructionProbe{status: "unavailable"}, nil
	}
	if err := ctx.Err(); err != nil {
		return nativeInstructionProbe{}, err
	}
	if reader.cache == nil {
		reader.cache = make(map[string]nativeInstructionCachedFile)
	}
	value := string(bytes)
	reader.cache[path] = nativeInstructionCachedFile{info: opened, content: value}
	reader.reads++
	return nativeInstructionProbe{status: "present", content: value}, nil
}

func renderNativeInstructionBaseline(files []nativeInstructionFile, replace bool) (string, []nativeInstructionFile) {
	intro := instructionrender.BaselineIntro
	if replace {
		intro = instructionrender.ReplacementIntro
		if len(files) == 0 {
			intro = instructionrender.EmptyReplacementIntro
		}
	}
	entries := make([]instructionrender.File, len(files))
	for index, file := range files {
		entries[index] = instructionrender.File{Path: file.display, Content: file.content}
	}
	rendered := instructionrender.Render(entries, nativeInstructionMaxBytes, intro, func(file instructionrender.File) string {
		return "Instructions from: " + file.Path + "\n\n" + file.Content
	})
	included := make([]nativeInstructionFile, len(rendered.Represented))
	for index, original := range rendered.Represented {
		included[index] = files[original]
	}
	return rendered.Text, included
}
