package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"go-bot/harness/agent"
)

const (
	rgRawLimit       = 20_000_000
	rgStderrLimit    = 64 << 10
	rgOutputLimit    = 512 << 10
	rgGlobMaxResults = 100
	rgGrepMaxMatches = 250
	rgGrepLineBytes  = 2000
)

var rgVCSExcludes = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// RipgrepSearch runs the same discovery program as DeepSeek's standard glob
// and grep tools, directly with an argv vector. Workdir is the Agent's native
// current directory, not a path restriction enforced by these tools.
type RipgrepSearch struct {
	Name    string
	Workdir string
	Binary  string // empty selects the ripgrep binary from PATH
}

type searchArgs struct {
	Pattern string  `json:"pattern"`
	Path    *string `json:"path"`
	Include *string `json:"include"`
}

type SearchToolError struct {
	Code string
	Err  error
}

func (e *SearchToolError) Error() string         { return e.Err.Error() }
func (e *SearchToolError) Unwrap() error         { return e.Err }
func (e *SearchToolError) ToolErrorCode() string { return e.Code }

func (t RipgrepSearch) Spec() agent.ToolSpec {
	switch t.Name {
	case "glob":
		return agent.ToolSpec{Name: "glob", Description: "Find files by glob pattern, including hidden and ignored files but excluding VCS metadata. Results are ordered by modification time; up to 100 paths appear inline, with the complete sorted result saved when larger. A pattern without '/' matches basenames at any depth.", Parameters: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`), ParallelSafe: true, Timeout: 30 * time.Second}
	case "grep":
		return agent.ToolSpec{Name: "grep", Description: "Search file contents with a ripgrep regular expression. Return matching lines with line numbers grouped by file; include is one positive glob filter. Up to 250 matches appear inline, with the complete result saved when larger.", Parameters: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"include":{"type":"string"}},"required":["pattern"]}`), ParallelSafe: true, Timeout: 30 * time.Second}
	default:
		panic("unknown ripgrep search tool")
	}
}

func (t RipgrepSearch) Validate(raw json.RawMessage) error {
	_, err := t.parse(raw)
	return err
}

func (t RipgrepSearch) parse(raw json.RawMessage) (searchArgs, error) {
	var args searchArgs
	if t.Name != "glob" && t.Name != "grep" {
		return args, errors.New("unknown ripgrep search tool")
	}
	if !filepath.IsAbs(t.Workdir) || t.Workdir == string(filepath.Separator) {
		return args, errors.New("Agent search working directory is not configured")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&args); err != nil {
		return args, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return args, errors.New("search arguments must contain exactly one JSON object")
	}
	if strings.ContainsRune(args.Pattern, 0) || (t.Name == "glob" && strings.TrimSpace(args.Pattern) == "") || (t.Name == "grep" && args.Pattern == "") {
		return args, errors.New("pattern must be a non-empty string without NUL")
	}
	if args.Path != nil && (strings.TrimSpace(*args.Path) == "" || strings.ContainsRune(*args.Path, 0)) {
		return args, errors.New("path must be non-empty when given")
	}
	if t.Name == "glob" && args.Include != nil {
		return args, errors.New("glob does not accept include")
	}
	if t.Name == "grep" && args.Include != nil {
		if strings.TrimSpace(*args.Include) == "" || strings.ContainsRune(*args.Include, 0) || strings.HasPrefix(*args.Include, "!") || containsCommaOutsideBraces(*args.Include) {
			return args, errors.New("include must be one positive glob; use {a,b} for alternation")
		}
	}
	return args, nil
}

func containsCommaOutsideBraces(pattern string) bool {
	depth := 0
	for _, r := range pattern {
		switch r {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

func (t RipgrepSearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := t.parse(raw)
	if err != nil {
		return "", err
	}
	argv := []string{"--no-config"}
	if t.Name == "glob" {
		argv = append(argv, "--files", "--glob="+args.Pattern, "--sort=modified", "--no-ignore", "--hidden")
		for _, name := range rgVCSExcludes {
			argv = append(argv, "--glob=!**/"+name, "--glob=!**/"+name+"/**")
		}
	} else {
		argv = append(argv, "--json", "--regexp="+args.Pattern)
		if args.Include != nil {
			argv = append(argv, "--glob="+*args.Include)
		}
	}
	if args.Path != nil {
		argv = append(argv, "--", *args.Path)
	}
	stdout, noMatches, err := t.run(ctx, argv)
	if err != nil {
		return "", err
	}
	if noMatches {
		if t.Name == "glob" {
			return "No files found", nil
		}
		return "No matches found", nil
	}
	if t.Name == "glob" {
		return t.renderGlob(stdout)
	}
	return t.renderGrep(stdout)
}

type boundedSearchOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedSearchOutput) Write(data []byte) (int, error) {
	if b.overflow {
		return 0, errors.New("raw search output overflow")
	}
	remaining := b.limit - b.buffer.Len()
	if len(data) > remaining {
		if remaining > 0 {
			_, _ = b.buffer.Write(data[:remaining])
		}
		b.overflow = true
		return max(remaining, 0), errors.New("raw search output overflow")
	}
	return b.buffer.Write(data)
}

type searchStderrTail struct{ data []byte }

func (t *searchStderrTail) Write(data []byte) (int, error) {
	count := len(data)
	t.data = append(t.data, data...)
	if len(t.data) > rgStderrLimit {
		t.data = append([]byte(nil), t.data[len(t.data)-rgStderrLimit:]...)
	}
	return count, nil
}

func (t RipgrepSearch) run(ctx context.Context, argv []string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, &SearchToolError{Code: "SEARCH_ABORTED", Err: err}
	}
	binary := t.Binary
	if binary == "" {
		binary = "rg"
	}
	command := exec.CommandContext(ctx, binary, argv...)
	command.Dir = t.Workdir
	command.Stdin = nil
	command.WaitDelay = 3 * time.Second
	stdout := &boundedSearchOutput{limit: rgRawLimit}
	stderr := &searchStderrTail{}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if ctx.Err() != nil {
		return nil, false, &SearchToolError{Code: "SEARCH_ABORTED", Err: fmt.Errorf("%s search aborted: %w", t.Name, ctx.Err())}
	}
	if stdout.overflow {
		return nil, false, &SearchToolError{Code: "SEARCH_RAW_OUTPUT_OVERFLOW", Err: fmt.Errorf("%s raw output exceeds %d bytes; narrow pattern or path", t.Name, rgRawLimit)}
	}
	if err == nil {
		return stdout.buffer.Bytes(), false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, true, nil
	}
	message := strings.TrimSpace(string(stderr.data))
	code := "SEARCH_FAILED"
	if strings.Contains(message, "regex parse error") || strings.Contains(message, "error parsing glob") {
		code = "SEARCH_INVALID_PATTERN"
	}
	return nil, false, &SearchToolError{Code: code, Err: fmt.Errorf("%s search failed: %w: %s", t.Name, err, message)}
}

func (t RipgrepSearch) renderGlob(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", &SearchToolError{Code: "SEARCH_FAILED", Err: errors.New("glob output is not UTF-8")}
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		if line != "" {
			paths = append(paths, workdirRelative(t.Workdir, line))
		}
	}
	if len(paths) == 0 {
		return "No files found", nil
	}
	shown := min(len(paths), rgGlobMaxResults)
	for shown > 0 && len(strings.Join(paths[:shown], "\n")) > rgOutputLimit {
		shown--
	}
	if shown == 0 {
		return "", &SearchToolError{Code: "SEARCH_FAILED", Err: errors.New("glob path exceeds result limit")}
	}
	if shown == len(paths) {
		return strings.Join(paths, "\n"), nil
	}
	fullPath := t.spill("glob-results-*.txt", strings.Join(paths, "\n"))
	footer := fmt.Sprintf("\n\n(Showing %d of %d paths. %s)", shown, len(paths), spillHint(fullPath, "Narrow pattern or path to see more."))
	return strings.Join(paths[:shown], "\n") + footer, nil
}

type rgJSONText struct {
	Text  *string `json:"text"`
	Bytes *string `json:"bytes"`
}

type rgJSONRecord struct {
	Type string `json:"type"`
	Data struct {
		Path       rgJSONText `json:"path"`
		LineNumber int        `json:"line_number"`
		Lines      rgJSONText `json:"lines"`
	} `json:"data"`
}

type grepMatch struct {
	path string
	line int
	text string
}

func (t RipgrepSearch) renderGrep(raw []byte) (string, error) {
	var matches []grepMatch
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), rgRawLimit+1)
	for scanner.Scan() {
		var record rgJSONRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return "", &SearchToolError{Code: "SEARCH_FAILED", Err: fmt.Errorf("malformed ripgrep JSON: %w", err)}
		}
		if record.Type != "match" {
			continue
		}
		if record.Data.Path.Text == nil || record.Data.LineNumber <= 0 || (record.Data.Lines.Text == nil && record.Data.Lines.Bytes == nil) {
			return "", &SearchToolError{Code: "SEARCH_FAILED", Err: errors.New("ripgrep JSON match lacks path, line number, or content")}
		}
		line := ""
		if record.Data.Lines.Text != nil {
			line = *record.Data.Lines.Text
		}
		if record.Data.Lines.Bytes != nil {
			line = "(line is not valid UTF-8)"
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		matches = append(matches, grepMatch{path: workdirRelative(t.Workdir, *record.Data.Path.Text), line: record.Data.LineNumber, text: previewSearchLine(line)})
	}
	if err := scanner.Err(); err != nil {
		return "", &SearchToolError{Code: "SEARCH_FAILED", Err: fmt.Errorf("read ripgrep JSON: %w", err)}
	}
	if len(matches) == 0 {
		return "No matches found", nil
	}
	full := formatGrep(matches)
	shown := min(len(matches), rgGrepMaxMatches)
	for shown > 0 && len(formatGrep(matches[:shown])) > rgOutputLimit {
		shown--
	}
	if shown == 0 {
		return "", &SearchToolError{Code: "SEARCH_FAILED", Err: errors.New("grep match exceeds result limit")}
	}
	if shown == len(matches) {
		return fmt.Sprintf("Found %d matches\n\n%s", len(matches), full), nil
	}
	fullPath := t.spill("grep-results-*.txt", fmt.Sprintf("Found %d matches\n\n%s", len(matches), full))
	return fmt.Sprintf("Found %d of %d matches\n\n%s\n\n(%s)", shown, len(matches), formatGrep(matches[:shown]), spillHint(fullPath, "Narrow pattern, path, or include to see more.")), nil
}

func formatGrep(matches []grepMatch) string {
	var out strings.Builder
	previous := ""
	for _, match := range matches {
		if match.path != previous {
			if out.Len() > 0 {
				out.WriteString("\n\n")
			}
			out.WriteString(match.path)
			out.WriteByte('\n')
			previous = match.path
		}
		fmt.Fprintf(&out, "Line %d: %s\n", match.line, match.text)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func previewSearchLine(line string) string {
	if len(line) <= rgGrepLineBytes {
		return line
	}
	cut := line[:rgGrepLineBytes]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

func workdirRelative(workdir, name string) string {
	if !filepath.IsAbs(name) {
		return strings.TrimPrefix(name, "."+string(filepath.Separator))
	}
	rel, err := filepath.Rel(workdir, name)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel
	}
	return name
}

func (t RipgrepSearch) spill(pattern, text string) string {
	dir := filepath.Join(t.Workdir, ".agent", "search-spill")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return ""
	}
	path := file.Name()
	if _, err = io.WriteString(file, text); err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		_ = os.Remove(path)
		return ""
	}
	return path
}

func spillHint(path, fallback string) string {
	if path == "" {
		return fallback
	}
	return "Full result stored at: " + path + ". Read the file to retrieve the complete result."
}
