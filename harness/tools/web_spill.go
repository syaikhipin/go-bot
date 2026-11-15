package tools

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

const webInlineTokens = 12_500

// retainWebText applies the shared upstream text-only spill policy at the
// Host tool-result boundary. The full rendering stays readable in the workspace.
func retainWebText(ctx context.Context, workdir, name, text string) string {
	if workdir == "" || webTextTokens(text) <= webInlineTokens || ctx.Err() != nil {
		return text
	}
	locator, err := saveWebText(workdir, name, text)
	if err != nil {
		return text
	}
	const gap = "\n\n[...]\n\n"
	notice := func(omitted int) string {
		return fmt.Sprintf("(Omitted %d bytes. Full formatted result stored at: %s. Use read with offset/limit, or grep this path to search within it.)", omitted, locator)
	}
	worstNotice := notice(len(text))
	if webTextTokens(worstNotice) > webInlineTokens {
		return text
	}
	available := max(0, webInlineTokens-webTextTokens(gap)-webTextTokens("\n\n"+worstNotice))
	headBudget := max(0, ((available+1)/2-4)*4)
	tailBudget := max(0, (available/2-4)*4)
	head := truncateUTF16(text, headBudget)
	tail := suffixUTF16(text[len(head):], tailBudget)
	footer := notice(len(text) - len(head) - len(tail))
	if head == "" && tail == "" {
		return footer
	}
	return head + gap + tail + "\n\n" + footer
}

func webTextTokens(text string) int { return (utf16Length(text)+3)/4 + 4 }

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

func saveWebText(workdir, name, text string) (string, error) {
	root, err := os.OpenRoot(workdir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	const dir = ".agent/web-spill"
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
