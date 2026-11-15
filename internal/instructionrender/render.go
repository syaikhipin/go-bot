// Package instructionrender implements the DeepSeek Harness workspace-instruction
// byte-budget renderer for both baseline and incremental instruction messages.
package instructionrender

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	BaselineIntro         = "The following workspace instructions may be relevant to your work. Use them as guidance when applicable. More specific instructions take precedence over broader ones. They do not override system, developer, or direct user instructions."
	ReplacementIntro      = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. " + BaselineIntro
	EmptyReplacementIntro = "This complete workspace instruction baseline replaces all earlier workspace instruction baselines. No workspace instructions are currently active."
	CompactIntro          = "Workspace instructions were omitted or truncated to fit the configured byte budget."
)

type File struct {
	Path    string
	Content string
}

type Result struct {
	Text        string
	Represented []int
}

type Section func(File) string

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	end := max(0, maxBytes)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func marker(maxBytes int, omitted []File, truncated *File, originalBytes, includedBytes int) string {
	if len(omitted) == 0 && truncated == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if len(omitted) > 0 {
		paths := make([]string, len(omitted))
		for index, file := range omitted {
			paths[index] = file.Path
		}
		parts = append(parts, "omitted "+strings.Join(paths, ", "))
	}
	if truncated != nil {
		parts = append(parts, fmt.Sprintf("truncated %s from %d to %d bytes", truncated.Path, originalBytes, includedBytes))
	}
	return fmt.Sprintf("Workspace instruction budget %d bytes: %s", maxBytes, strings.Join(parts, "; "))
}

func frame(files []File, maxBytes int, omitted []File, truncated *File, originalBytes, includedBytes int, intro string, section Section) string {
	blocks := make([]string, 0, len(files)+2)
	if text := marker(maxBytes, omitted, truncated, originalBytes, includedBytes); text != "" {
		blocks = append(blocks, text)
	}
	if intro != "" {
		blocks = append(blocks, intro)
	}
	for _, file := range files {
		if text := section(file); text != "" {
			blocks = append(blocks, text)
		}
	}
	body := strings.ReplaceAll(strings.Join(blocks, "\n\n"), "</system-reminder>", "<\\/system-reminder>")
	return "<system-reminder>\n" + body + "\n</system-reminder>"
}

func truncateToFit(file File, maxBytes int, omitted []File, intro string, section Section) File {
	low, high := 0, len(file.Content)
	best := file
	best.Content = ""
	for low <= high {
		mid := (low + high) / 2
		candidate := file
		candidate.Content = truncateUTF8(file.Content, mid)
		text := frame([]File{candidate}, maxBytes, omitted, &candidate, len(file.Content), len(candidate.Content), intro, section)
		if len(text) <= maxBytes {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best
}

// Render follows upstream's broadest-first omission, most-specific truncation,
// compact-intro fallback, and represented-source semantics. Its output is never
// larger than maxBytes, even when a heading alone exceeds the budget.
func Render(files []File, maxBytes int, intro string, section Section) Result {
	if maxBytes <= 0 {
		return Result{}
	}
	if full := frame(files, maxBytes, nil, nil, 0, 0, intro, section); len(full) <= maxBytes {
		indices := make([]int, len(files))
		for index := range files {
			indices[index] = index
		}
		return Result{Text: full, Represented: indices}
	}
	for start := 1; start < len(files); start++ {
		if suffix := frame(files[start:], maxBytes, files[:start], nil, 0, 0, intro, section); len(suffix) <= maxBytes {
			indices := make([]int, len(files)-start)
			for index := range indices {
				indices[index] = start + index
			}
			return Result{Text: suffix, Represented: indices}
		}
	}
	if len(files) == 0 {
		return Result{}
	}
	index := len(files) - 1
	last := files[index]
	omitted := files[:index]
	for _, candidateIntro := range []string{intro, CompactIntro} {
		candidate := truncateToFit(last, maxBytes, omitted, candidateIntro, section)
		text := frame([]File{candidate}, maxBytes, omitted, &candidate, len(last.Content), len(candidate.Content), candidateIntro, section)
		if len(text) <= maxBytes {
			result := Result{Text: text}
			if len(candidate.Content) > 0 || len(last.Content) == 0 {
				result.Represented = []int{index}
			}
			return result
		}
	}
	zero := last
	zero.Content = ""
	notice := strings.ReplaceAll(marker(maxBytes, omitted, &zero, len(last.Content), 0), "</system-reminder>", "<\\/system-reminder>")
	withHeading := strings.ReplaceAll(notice+"\n\n"+section(zero), "</system-reminder>", "<\\/system-reminder>")
	if len(withHeading) <= maxBytes {
		result := Result{Text: withHeading}
		if len(last.Content) == 0 {
			result.Represented = []int{index}
		}
		return result
	}
	if len(notice) > maxBytes {
		notice = truncateUTF8(notice, maxBytes)
	}
	return Result{Text: notice}
}
