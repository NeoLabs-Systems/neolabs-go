package llm

import (
	"regexp"
	"strings"
)

// Messages carry mixed content in the OpenAI shape: a list of
// {"type":"text","text":...} and {"type":"image_url","image_url":{"url":...}}
// parts. Providers with another wire format translate it with these helpers.

var dataURL = regexp.MustCompile(`(?s)^data:([^;,]+);base64,(.*)$`)

// ImagePart reads an image_url part holding a base64 data URL.
func ImagePart(part any) (mimeType, data string, ok bool) {
	fields, _ := part.(map[string]any)
	if fields["type"] != "image_url" {
		return "", "", false
	}
	image, _ := fields["image_url"].(map[string]any)
	url, _ := image["url"].(string)
	match := dataURL.FindStringSubmatch(url)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

// TextPart reads a text part.
func TextPart(part any) (string, bool) {
	fields, _ := part.(map[string]any)
	if fields["type"] != "text" {
		return "", false
	}
	text, _ := fields["text"].(string)
	return text, true
}

// ContentText is the text of a message's content, a string or a list of parts.
func ContentText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts, _ := content.([]any)
	var texts []string
	for _, part := range parts {
		if text, ok := TextPart(part); ok {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}
