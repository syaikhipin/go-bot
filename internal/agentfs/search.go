package agentfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	searchMaxDepth      = 12
	searchMaxEntries    = 2000
	searchMaxDirEntries = 1000
	searchMaxFileBytes  = 1 << 20
	searchMaxTotalBytes = 32 << 20
)

type SearchOptions struct {
	Path       string
	Pattern    string
	Kind       string
	MaxMatches int
}

type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

type SearchResult struct {
	Matches      []SearchMatch `json:"matches"`
	ScannedFiles int           `json:"scannedFiles"`
	Truncated    bool          `json:"truncated"`
	Reason       string        `json:"reason,omitempty"`
}

type searchFileSystem struct {
	lstat func(string) (os.FileInfo, error)
	open  func(string) (*os.File, error)
}

// searchFiles never invokes a shell. The caller supplies either an os.Root
// confined filesystem or the container's native filesystem. Symlinks are not
// traversed; count/depth/size caps make broad searches bounded and explicit.
func searchFiles(ctx context.Context, fs searchFileSystem, backendRoot, visibleRoot string, options SearchOptions) (SearchResult, error) {
	result := SearchResult{Matches: make([]SearchMatch, 0)}
	if options.Kind == "" {
		options.Kind = "content"
	}
	if options.Kind != "content" && options.Kind != "name" {
		return result, errors.New("search kind must be content or name")
	}
	if options.MaxMatches <= 0 || options.MaxMatches > 100 || options.Pattern == "" || len(options.Pattern) > 512 {
		return result, errors.New("search requires a regular expression of at most 512 bytes and 1-100 matches")
	}
	pattern, err := regexp.Compile(options.Pattern)
	if err != nil {
		return result, fmt.Errorf("invalid search regular expression: %w", err)
	}
	visited := 0
	scannedBytes := int64(0)
	hardStop := false
	markTruncated := func(reason string) {
		result.Truncated = true
		if result.Reason == "" {
			result.Reason = reason
		}
	}
	var walk func(string, string, int) error
	walk = func(backendPath, visiblePath string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if hardStop {
			return nil
		}
		if visited >= searchMaxEntries {
			markTruncated("entry limit")
			result.Reason = "entry limit"
			hardStop = true
			return nil
		}
		visited++
		info, err := fs.lstat(backendPath)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			if depth >= searchMaxDepth {
				markTruncated("depth limit")
				return nil
			}
			file, err := fs.open(backendPath)
			if err != nil {
				return err
			}
			names, readErr := file.Readdirnames(searchMaxDirEntries + 1)
			closeErr := file.Close()
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if len(names) > searchMaxDirEntries {
				markTruncated("directory entry limit")
				names = names[:searchMaxDirEntries]
			}
			slices.Sort(names)
			for _, name := range names {
				if isVCSDirectory(name) {
					continue
				}
				if err := walk(filepath.Join(backendPath, name), filepath.Join(visiblePath, name), depth+1); err != nil {
					return err
				}
				if hardStop {
					break
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		result.ScannedFiles++
		if options.Kind == "name" {
			if pattern.MatchString(visiblePath) {
				result.Matches = append(result.Matches, SearchMatch{Path: visiblePath})
			}
		} else {
			if info.Size() > searchMaxFileBytes {
				markTruncated("large file skipped")
				return nil
			}
			if scannedBytes+info.Size() > searchMaxTotalBytes {
				markTruncated("byte budget")
				result.Reason = "byte budget"
				hardStop = true
				return nil
			}
			file, err := fs.open(backendPath)
			if err != nil {
				return err
			}
			data, readErr := io.ReadAll(io.LimitReader(file, searchMaxFileBytes+1))
			closeErr := file.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
			if len(data) > searchMaxFileBytes {
				markTruncated("large file skipped")
				return nil
			}
			scannedBytes += int64(len(data))
			if scannedBytes > searchMaxTotalBytes {
				markTruncated("byte budget")
				result.Reason = "byte budget"
				hardStop = true
				return nil
			}
			if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
				return nil
			}
			scanner := bufio.NewScanner(bytes.NewReader(data))
			scanner.Buffer(make([]byte, 4096), searchMaxFileBytes+1)
			for lineNumber := 1; scanner.Scan(); lineNumber++ {
				if lineNumber%64 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				line := scanner.Text()
				if !pattern.MatchString(line) {
					continue
				}
				result.Matches = append(result.Matches, SearchMatch{Path: visiblePath, Line: lineNumber, Text: searchPreview(line)})
				if len(result.Matches) >= options.MaxMatches {
					markTruncated("match limit")
					result.Reason = "match limit"
					hardStop = true
					break
				}
			}
			if err := scanner.Err(); err != nil {
				return err
			}
		}
		if len(result.Matches) >= options.MaxMatches {
			markTruncated("match limit")
			result.Reason = "match limit"
			hardStop = true
		}
		return nil
	}
	if err := walk(backendRoot, visibleRoot, 0); err != nil {
		return SearchResult{}, err
	}
	return result, nil
}

func isVCSDirectory(name string) bool {
	switch name {
	case ".git", ".svn", ".hg", ".bzr", ".jj", ".sl":
		return true
	default:
		return false
	}
}

func searchPreview(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if len(line) <= 300 {
		return line
	}
	preview := line[:300]
	for !utf8.ValidString(preview) {
		preview = preview[:len(preview)-1]
	}
	return preview + "…"
}
