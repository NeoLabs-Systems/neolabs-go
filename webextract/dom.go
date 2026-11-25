package webextract

import (
	"strings"

	"golang.org/x/net/html"
)

func isElement(n *html.Node) bool { return n != nil && n.Type == html.ElementNode }

func tagName(n *html.Node) string { return strings.ToLower(n.Data) }

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func attrOr(n *html.Node, name string) string {
	v, _ := attr(n, name)
	return v
}

// walk visits elements in document order; returning false skips children.
func walk(n *html.Node, visit func(*html.Node) bool) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if isElement(c) {
			if visit(c) {
				walk(c, visit)
			}
		} else {
			walk(c, visit)
		}
		c = next
	}
}

// all returns the descendants of n that match, in document order.
func all(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	walk(n, func(e *html.Node) bool {
		if match(e) {
			out = append(out, e)
		}
		return true
	})
	return out
}

func first(n *html.Node, match func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(n, func(e *html.Node) bool {
		if found != nil {
			return false
		}
		if match(e) {
			found = e
			return false
		}
		return true
	})
	return found
}

func byTag(tag string) func(*html.Node) bool {
	return func(n *html.Node) bool { return tagName(n) == tag }
}

func remove(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// text is cheerio's .text(): every text node below n, scripts included.
func text(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(c *html.Node) {
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			switch k.Type {
			case html.TextNode:
				b.WriteString(k.Data)
			case html.ElementNode:
				rec(k)
			}
		}
	}
	rec(n)
	return b.String()
}

func textOf(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(text(n))
	}
	return b.String()
}

// innerRaw is the unescaped content of a raw-text element such as a script.
func innerRaw(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

var voidElements = map[string]bool{
	"area": true, "base": true, "basefont": true, "bgsound": true, "br": true, "col": true, "embed": true,
	"frame": true, "hr": true, "img": true, "input": true, "keygen": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

var rawTextElements = map[string]bool{
	"style": true, "script": true, "xmp": true, "iframe": true, "noembed": true, "noframes": true, "plaintext": true,
}

var (
	textEscaper = strings.NewReplacer("&", "&amp;", " ", "&nbsp;", "<", "&lt;", ">", "&gt;")
	attrEscaper = strings.NewReplacer("&", "&amp;", " ", "&nbsp;", `"`, "&quot;")
)

// outerHTMLLength is the length of parse5's serialisation of n, which is all
// the content scoring needs.
func outerHTMLLength(n *html.Node) int {
	var b strings.Builder
	serialize(&b, n)
	return utf16Len(b.String())
}

func serialize(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		if p := n.Parent; p != nil && isElement(p) && (rawTextElements[tagName(p)] || tagName(p) == "noscript") {
			b.WriteString(n.Data)
		} else {
			b.WriteString(textEscaper.Replace(n.Data))
		}
	case html.CommentNode:
		b.WriteString("<!--" + n.Data + "-->")
	case html.ElementNode:
		b.WriteString("<" + n.Data)
		for _, a := range n.Attr {
			name := a.Key
			if a.Namespace != "" {
				name = a.Namespace + ":" + a.Key
			}
			b.WriteString(" " + name + `="` + attrEscaper.Replace(a.Val) + `"`)
		}
		b.WriteString(">")
		if voidElements[tagName(n)] {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			serialize(b, c)
		}
		b.WriteString("</" + n.Data + ">")
	default:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			serialize(b, c)
		}
	}
}
