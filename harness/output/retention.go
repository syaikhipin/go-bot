package output

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf16"
	"unicode/utf8"
)

const InlineTokens = 12_500

// RetainText returns a token-bounded preview backed by an exact readable copy.
// On unavailable storage the original content is returned with an error, so
// callers can diagnose degraded retention without losing a successful result.
func RetainText(ctx context.Context, workdir, directory, name, text string, maxTokens int) (string, error) {
	if Estimate(text) <= maxTokens {
		return text, nil
	}
	if err := ctx.Err(); err != nil {
		return text, err
	}
	if workdir == "" {
		return text, errors.New("tool output has no recovery workspace")
	}
	const gap = "\n\n[...]\n\n"
	locator, err := saveText(workdir, directory, name, text)
	if err != nil {
		return text, err
	}
	notice := func(omitted int) string {
		return fmt.Sprintf("(Omitted %d bytes. Full formatted result stored at: %s. Use readfile with offset/limit, or grep this path to search within it.)", omitted, locator)
	}
	worstNotice := notice(len(text))
	if Estimate(worstNotice) > maxTokens {
		return text, errors.New("recovery notice exceeds tool output budget")
	}
	available := max(0, maxTokens-Estimate(gap)-Estimate("\n\n"+worstNotice))
	head := truncateUTF16(text, max(0, ((available+1)/2-4)*4))
	tail := suffixUTF16(text[len(head):], max(0, (available/2-4)*4))
	footer := notice(len(text) - len(head) - len(tail))
	if head == "" && tail == "" {
		return footer, nil
	}
	return head + gap + tail + "\n\n" + footer, nil
}

func Estimate(text string) int { return (utf16Length(text)+3)/4 + 4 }

func suffixUTF16(text string, maxChars int) string {
	chars := 0
	for i := len(text); i > 0; {
		r, bytes := utf8.DecodeLastRuneInString(text[:i])
		chars += utf16.RuneLen(r)
		if chars > maxChars {
			return text[i:]
		}
		i -= bytes
	}
	return text
}

func saveText(workdir, dir, name, text string) (string, error) {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, hex.EncodeToString(id[:])+"-"+name+".txt")
	file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := io.WriteString(file, text)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		_ = root.Remove(path)
		return "", err
	}
	return filepath.Join(workdir, path), nil
}

func utf16Length(text string) int {
	n := 0
	for _, r := range text {
		n += utf16.RuneLen(r)
	}
	return n
}
func truncateUTF16(text string, maxChars int) string {
	count := 0
	for index, r := range text {
		count += utf16.RuneLen(r)
		if count > maxChars {
			return text[:index]
		}
	}
	return text
}
