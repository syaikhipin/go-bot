package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf16"
)

// PublicToolName follows DeepSeek Harness's pinned naming contract. JS's
// non-Unicode regexp replaces UTF-16 units, including both halves of an emoji.
// Wire calls always retain the separately stored raw name.
func PublicToolName(server, raw string) string {
	joined := "mcp__" + server + "__" + raw
	units := utf16.Encode([]rune(joined))
	normalized := make([]byte, len(units))
	clean := true
	for i, unit := range units {
		if unit >= 'a' && unit <= 'z' || unit >= 'A' && unit <= 'Z' || unit >= '0' && unit <= '9' || unit == '_' || unit == '-' {
			normalized[i] = byte(unit)
		} else {
			normalized[i] = '_'
			clean = false
		}
	}
	if clean && len(normalized) <= 64 {
		return string(normalized)
	}
	sum := sha256.Sum256([]byte(server + "\x00" + raw))
	suffix := hex.EncodeToString(sum[:])[:12]
	if len(normalized) > 51 {
		normalized = normalized[:51]
	}
	return string(normalized) + "_" + suffix
}
