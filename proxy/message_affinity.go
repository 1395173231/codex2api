package proxy

import (
	"hash/fnv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const (
	minMessageAffinityRunes  = 16
	maxMessageAffinityBytes  = 4096
	maxMessageAffinityHashes = 64
)

// deriveMessageAffinityHashes extracts privacy-preserving fingerprints from
// user-authored text across Chat Completions, Responses, and Anthropic-shaped
// request bodies. Raw text never leaves this function.
func deriveMessageAffinityHashes(body []byte) []uint64 {
	if len(body) == 0 {
		return nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil
	}
	hashes := make([]uint64, 0, 16)
	seen := make(map[uint64]struct{}, 16)
	appendText := func(text string) bool {
		normalized := normalizeMessageAffinityText(text)
		if normalized == "" {
			return true
		}
		hasher := fnv.New64a()
		_, _ = hasher.Write([]byte(normalized))
		hash := hasher.Sum64()
		if hash == 0 {
			return true
		}
		if _, ok := seen[hash]; ok {
			return true
		}
		seen[hash] = struct{}{}
		hashes = append(hashes, hash)
		return len(hashes) < maxMessageAffinityHashes
	}

	appendContent := func(content gjson.Result) bool {
		switch {
		case content.Type == gjson.String:
			return appendText(content.String())
		case content.IsArray():
			keepGoing := true
			content.ForEach(func(_, part gjson.Result) bool {
				if !keepGoing {
					return false
				}
				if part.Type == gjson.String {
					keepGoing = appendText(part.String())
					return keepGoing
				}
				if text := part.Get("text"); text.Type == gjson.String {
					keepGoing = appendText(text.String())
					return keepGoing
				}
				if nested := part.Get("content"); nested.Type == gjson.String {
					keepGoing = appendText(nested.String())
				}
				return keepGoing
			})
			return keepGoing
		default:
			return true
		}
	}

	// Message-style APIs take precedence when both fields happen to exist.
	if messages := root.Get("messages"); messages.IsArray() {
		keepGoing := true
		messages.ForEach(func(_, message gjson.Result) bool {
			if !keepGoing {
				return false
			}
			if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "user") {
				return true
			}
			keepGoing = appendContent(message.Get("content"))
			return keepGoing
		})
		return hashes
	}

	input := root.Get("input")
	if !input.Exists() {
		return nil
	}
	if input.Type == gjson.String {
		appendText(input.String())
		return hashes
	}
	if !input.IsArray() {
		return nil
	}
	keepGoing := true
	input.ForEach(func(_, item gjson.Result) bool {
		if !keepGoing {
			return false
		}
		if item.Type == gjson.String {
			keepGoing = appendText(item.String())
			return keepGoing
		}
		role := strings.TrimSpace(item.Get("role").String())
		if role != "" && !strings.EqualFold(role, "user") {
			return true
		}
		if role != "" {
			keepGoing = appendContent(item.Get("content"))
			return keepGoing
		}
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if itemType != "" && itemType != "input_text" && itemType != "message" {
			return true
		}
		if text := item.Get("text"); text.Type == gjson.String {
			keepGoing = appendText(text.String())
			return keepGoing
		}
		keepGoing = appendContent(item.Get("content"))
		return keepGoing
	})
	return hashes
}

func normalizeMessageAffinityText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return ""
	}
	var normalized strings.Builder
	normalized.Grow(256)
	previousSpace := false
	alphanumericRunes := 0
	for _, r := range text {
		if r == '\u0000' || r == '\uFEFF' {
			continue
		}
		if unicode.IsSpace(r) {
			if !previousSpace && normalized.Len() < maxMessageAffinityBytes {
				normalized.WriteByte(' ')
				previousSpace = true
			}
			continue
		}
		runeBytes := utf8.RuneLen(r)
		if runeBytes < 0 || normalized.Len()+runeBytes > maxMessageAffinityBytes {
			break
		}
		previousSpace = false
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alphanumericRunes++
		}
		normalized.WriteRune(r)
	}
	if alphanumericRunes < minMessageAffinityRunes {
		return ""
	}
	return strings.TrimSpace(normalized.String())
}
