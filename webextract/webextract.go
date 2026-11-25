// Package webextract turns a web page into compact Markdown for a language
// model: noise removal, readability-style main-content selection, Markdown
// conversion, a link list and structured-data fallbacks. It reproduces the
// NeoAgent Node extractor (cheerio based) output for output.
package webextract

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Metadata describes the page.
type Metadata struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Author      string `json:"author"`
	URL         string `json:"url"`
}

// Result is the extracted page.
type Result struct {
	Markdown   string   `json:"markdown"`
	Metadata   Metadata `json:"metadata"`
	WordCount  int      `json:"wordCount"`
	Strategies []string `json:"strategies"`
}

var (
	noiseTags      = []string{"script", "style", "noscript", "nav", "header", "footer", "aside", "iframe", "dialog", "template"}
	noiseARIARoles = map[string]bool{"navigation": true, "banner": true, "contentinfo": true, "complementary": true}
	noiseTokens    = map[string]bool{
		"sidebar": true, "nav": true, "navbar": true, "navigation": true, "modal": true, "popup": true, "cookie": true,
		"newsletter": true, "menu": true, "breadcrumb": true, "breadcrumbs": true, "social": true, "share": true,
		"widget": true, "promo": true, "banner": true, "ad": true, "ads": true, "advertisement": true, "overlay": true,
		"toast": true, "tooltip": true, "flyout": true, "drawer": true, "offcanvas": true, "skip-link": true,
		"toolbar": true, "ribbon": true, "announcement": true, "notice": true,
	}
	scoreBoost    = map[string]bool{"article": true, "main": true}
	scorePenalty  = map[string]bool{"nav": true, "aside": true, "footer": true, "header": true}
	candidateTags = map[string]bool{"article": true, "main": true, "div": true, "section": true, "td": true}
	schemaContent = map[string]bool{
		"Article": true, "NewsArticle": true, "BlogPosting": true, "Product": true, "Recipe": true,
		"FAQPage": true, "HowTo": true, "Event": true, "Review": true, "Course": true,
	}
	schemaSkip       = map[string]bool{"WebSite": true, "WebPage": true, "SiteNavigationElement": true, "BreadcrumbList": true, "SearchAction": true}
	schemaLongFields = map[string]bool{"articleBody": true, "body": true, "text": true, "description": true}
	blockTags        = map[string]bool{
		"p": true, "div": true, "section": true, "article": true, "blockquote": true, "pre": true,
		"ul": true, "ol": true, "li": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"table": true, "tr": true, "td": true, "th": true, "thead": true, "tbody": true, "tfoot": true,
		"figure": true, "figcaption": true, "details": true, "summary": true,
	}
	genericAlt = map[string]bool{"logo": true, "icon": true, "image": true, "img": true, "photo": true, "picture": true, "banner": true, "thumbnail": true}

	languageClass = regexp.MustCompile(`language-([^` + jsSpaceClass + `]+)`)
	cssToken      = regexp.MustCompile(`^[a-z-]+(-[a-z0-9]+)*$`)
	proseSignal   = regexp.MustCompile(`[A-Z]|[.!?,;:]|[0-9]{4}|[` + jsSpaceClass + `][a-z]{4,}[` + jsSpaceClass + `]`)
	statOnly      = regexp.MustCompile(`(?i)^[0-9,.+%$€£¥km bBMKGT/×x-]+$`)
	fence         = regexp.MustCompile("^```")
	boldPair      = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	italicPair    = regexp.MustCompile(`\*([^*\n]+)\*`)
	paragraphGap  = regexp.MustCompile(`\n{2,}`)
	trailingBlank = regexp.MustCompile(`[ \t]+\n`)
	manyNewlines  = regexp.MustCompile(`\n{3,}`)
	headingTag    = regexp.MustCompile(`^h([1-6])$`)
)

func hasNoiseToken(value string) bool {
	if value == "" {
		return false
	}
	for _, token := range jsSpaceRun.Split(value, -1) {
		if noiseTokens[strings.ToLower(token)] {
			return true
		}
	}
	return false
}

func removeNoise(doc *html.Node) {
	for _, tag := range noiseTags {
		for _, n := range all(doc, byTag(tag)) {
			remove(n)
		}
	}
	for _, n := range all(doc, func(n *html.Node) bool { _, ok := attr(n, "role"); return ok }) {
		if noiseARIARoles[strings.ToLower(attrOr(n, "role"))] {
			remove(n)
		}
	}
	walk(doc, func(n *html.Node) bool {
		if hasNoiseToken(attrOr(n, "class")) || hasNoiseToken(attrOr(n, "id")) {
			remove(n)
			return false
		}
		return true
	})
}

func metaContent(doc *html.Node, name string) string {
	for _, key := range []string{"name", "property"} {
		m := first(doc, func(n *html.Node) bool {
			v, ok := attr(n, key)
			return tagName(n) == "meta" && ok && v == name
		})
		if m != nil {
			if v := attrOr(m, "content"); v != "" {
				return v
			}
		}
	}
	return ""
}

func extractMetadata(doc *html.Node, baseURL string) Metadata {
	firstOf := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}
	return Metadata{
		Title:       firstOf(metaContent(doc, "og:title"), metaContent(doc, "twitter:title"), jsTrim(textOf(all(doc, byTag("title"))))),
		Description: firstOf(metaContent(doc, "og:description"), metaContent(doc, "twitter:description"), metaContent(doc, "description")),
		Author:      metaContent(doc, "author"),
		URL:         firstOf(metaContent(doc, "og:url"), baseURL),
	}
}

func textLength(n *html.Node) int { return utf16Len(jsTrim(collapse(text(n)))) }

func linkDensity(n *html.Node) float64 {
	total := textLength(n)
	if total == 0 {
		return 0
	}
	links := 0
	for _, a := range all(n, byTag("a")) {
		links += utf16Len(jsTrim(text(a)))
	}
	return float64(links) / float64(total)
}

func scoreElement(n *html.Node) float64 {
	length := textLength(n)
	if length < 20 {
		return 0
	}
	score := 0.0
	if outer := outerHTMLLength(n); outer > 0 {
		score = float64(length) / float64(outer)
	}
	if ld := linkDensity(n); ld > 0.3 {
		score *= 1 - ld
	}
	tag := tagName(n)
	if scoreBoost[tag] {
		score *= 1.25
	}
	if scorePenalty[tag] {
		score *= 0.75
	}
	return score
}

func isMainRole(n *html.Node) bool { v, ok := attr(n, "role"); return ok && v == "main" }

func body(doc *html.Node) *html.Node {
	return first(doc, byTag("body"))
}

func selectMainContent(doc *html.Node) *html.Node {
	for _, match := range []func(*html.Node) bool{byTag("article"), byTag("main"), isMainRole} {
		if n := first(doc, match); n != nil && wordCount(text(n)) > 100 {
			return n
		}
	}
	var best *html.Node
	bestScore := 0.0
	for _, n := range all(doc, func(n *html.Node) bool { return candidateTags[tagName(n)] || isMainRole(n) }) {
		if s := scoreElement(n); s > bestScore {
			best, bestScore = n, s
		}
	}
	if best != nil {
		return best
	}
	return body(doc)
}

func cleanSchemaItem(item *value) *value {
	if item.kind != 'o' {
		return item
	}
	out := &value{kind: 'o'}
	for i, k := range item.keys {
		v := item.vals[i]
		if schemaLongFields[k] && v.kind == 's' && utf16Len(v.str) > 500 {
			continue
		}
		out.set(k, v)
	}
	return out
}

func extractStructuredData(doc *html.Node) []*value {
	var items []*value
	total := 0
	const sizeCap = 16 * 1024
	for _, script := range all(doc, func(n *html.Node) bool {
		v, ok := attr(n, "type")
		return tagName(n) == "script" && ok && v == "application/ld+json"
	}) {
		if total >= sizeCap {
			continue
		}
		parsed, err := parseJSON(innerRaw(script))
		if err != nil {
			continue
		}
		list := []*value{parsed}
		if parsed.kind == 'a' {
			list = parsed.vals
		}
		for _, item := range list {
			if item.kind == 'z' {
				break // reading a property of null throws and ends this script
			}
			typ := item.get("@type")
			if !typ.truthy() || typ.kind != 's' || schemaSkip[typ.str] || !schemaContent[typ.str] {
				continue
			}
			cleaned := cleanSchemaItem(item)
			size := utf16Len(cleaned.stringify(0))
			if total+size > sizeCap {
				continue
			}
			items = append(items, cleaned)
			total += size
		}
	}
	return items
}

func extractNextData(doc *html.Node) *value {
	n := first(doc, func(n *html.Node) bool { v, ok := attr(n, "id"); return ok && v == "__NEXT_DATA__" })
	if n == nil {
		return nil
	}
	raw := innerRaw(n)
	if raw == "" {
		return nil
	}
	parsed, err := parseJSON(raw)
	if err != nil {
		return nil
	}
	props := parsed.get("props").get("pageProps")
	if !props.truthy() || props.kind != 'o' {
		return nil
	}
	rest := &value{kind: 'o'}
	for i, k := range props.keys {
		if k != "buildId" && k != "isFallback" {
			rest.set(k, props.vals[i])
		}
	}
	if len(rest.keys) == 0 {
		return nil
	}
	return rest
}

// resolveURL is new URL(href, base).toString(), or nil for fragments and
// javascript: links.
func resolveURL(href, base string) (string, bool) {
	if href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "#") {
		return "", false
	}
	b, err := url.Parse(base)
	if err != nil || b.Scheme == "" {
		if r, err := url.Parse(href); err == nil && r.Scheme != "" && r.Host != "" {
			return normalizeURL(r), true
		}
		return href, true
	}
	r, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return href, true
	}
	return normalizeURL(b.ResolveReference(r)), true
}

func normalizeURL(u *url.URL) string {
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
	}
	if u.Host != "" && u.Path == "" && u.Opaque == "" {
		u.Path = "/"
	}
	return u.String()
}

func isMeaningfulAlt(alt string) bool {
	if alt == "" || utf16Len(alt) <= 10 {
		return false
	}
	return !genericAlt[jsTrim(strings.ToLower(alt))]
}

type link struct{ text, href string }

type mdContext struct {
	baseURL   string
	links     []link
	listStack []string
	listIndex int
	logos     []string
}

func (ctx *mdContext) children(n *html.Node, depth int) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(ctx.node(c, depth+1))
	}
	return b.String()
}

func (ctx *mdContext) node(n *html.Node, depth int) string {
	if depth > 100 {
		if n.Type == html.TextNode {
			return n.Data
		}
		return ""
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type != html.ElementNode {
		return ""
	}
	tag := tagName(n)
	if m := headingTag.FindStringSubmatch(tag); m != nil {
		level, _ := strconv.Atoi(m[1])
		t := jsTrim(text(n))
		if t == "" {
			return ""
		}
		return "\n\n" + strings.Repeat("#", level) + " " + t + "\n\n"
	}
	switch tag {
	case "p":
		if inner := jsTrim(ctx.children(n, depth)); inner != "" {
			return "\n\n" + inner + "\n\n"
		}
		return ""
	case "br":
		return "\n"
	case "hr":
		return "\n\n---\n\n"
	case "blockquote":
		inner := jsTrim(ctx.children(n, depth))
		if inner == "" {
			return ""
		}
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			lines[i] = "> " + l
		}
		return "\n\n" + strings.Join(lines, "\n") + "\n\n"
	case "pre":
		code := first(n, byTag("code"))
		class := ""
		if code != nil {
			class = attrOr(code, "class")
		}
		if class == "" {
			class = attrOr(n, "class")
		}
		lang := ""
		if m := languageClass.FindStringSubmatch(class); m != nil {
			lang = m[1]
		}
		source := n
		if code != nil {
			source = code
		}
		return "\n\n```" + lang + "\n" + jsTrim(text(source)) + "\n```\n\n"
	case "code":
		return "`" + text(n) + "`"
	case "strong", "b":
		if inner := jsTrim(ctx.children(n, depth)); inner != "" {
			return "**" + inner + "**"
		}
		return ""
	case "em", "i":
		if inner := jsTrim(ctx.children(n, depth)); inner != "" {
			return "*" + inner + "*"
		}
		return ""
	case "a":
		href, ok := resolveURL(attrOr(n, "href"), ctx.baseURL)
		t := jsTrim(ctx.children(n, depth))
		if t == "" {
			t = jsTrim(text(n))
		}
		if t == "" {
			return ""
		}
		if ok && href != "" {
			ctx.links = append(ctx.links, link{t, href})
		}
		return t
	case "img":
		alt := jsTrim(attrOr(n, "alt"))
		if isMeaningfulAlt(alt) {
			return alt
		}
		if alt != "" && genericAlt[strings.ToLower(alt)] {
			ctx.logos = append(ctx.logos, alt)
		}
		return ""
	case "ul", "ol":
		ctx.listStack = append(ctx.listStack, tag)
		inner := jsTrim(ctx.children(n, depth))
		ctx.listStack = ctx.listStack[:len(ctx.listStack)-1]
		if inner != "" {
			return "\n\n" + inner + "\n\n"
		}
		return ""
	case "li":
		ordered := len(ctx.listStack) > 0 && ctx.listStack[len(ctx.listStack)-1] == "ol"
		inner := jsTrim(ctx.children(n, depth))
		if inner == "" {
			return ""
		}
		if ordered {
			ctx.listIndex++
			return strconv.Itoa(ctx.listIndex) + ". " + inner + "\n"
		}
		return "- " + inner + "\n"
	case "table":
		return "\n\n" + jsTrim(collapse(text(n))) + "\n\n"
	case "summary":
		return "**" + jsTrim(ctx.children(n, depth)) + "**\n"
	case "figcaption":
		if inner := jsTrim(ctx.children(n, depth)); inner != "" {
			return "\n*" + inner + "*\n"
		}
		return ""
	}
	if blockTags[tag] {
		if inner := ctx.children(n, depth); inner != "" {
			return "\n" + inner + "\n"
		}
		return ""
	}
	return ctx.children(n, depth)
}

func htmlToMarkdown(root *html.Node, baseURL string) (string, []link, []string) {
	ctx := &mdContext{baseURL: baseURL}
	var b strings.Builder
	if root != nil {
		for c := root.FirstChild; c != nil; c = c.NextSibling {
			b.WriteString(ctx.node(c, 0))
		}
	}
	return b.String(), ctx.links, ctx.logos
}

func isCSSClassLine(line string) bool {
	tokens := jsSpaceRun.Split(jsTrim(line), -1)
	if len(tokens) < 3 {
		return false
	}
	for _, t := range tokens {
		if !cssToken.MatchString(t) {
			return false
		}
	}
	return !proseSignal.MatchString(line)
}

func mergeStats(lines []string) []string {
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if i+1 < len(lines) {
			next := jsTrim(lines[i+1])
			if statOnly.MatchString(jsTrim(line)) && next != "" && len(strings.Split(next, " ")) <= 4 {
				out = append(out, jsTrim(line)+" "+next)
				i++
				continue
			}
		}
		out = append(out, line)
	}
	return out
}

// outsideFences applies fn to each line that is not inside a ``` block.
func outsideFences(text string, fn func(string) (string, bool)) string {
	var out []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if fence.MatchString(jsTrim(line)) {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if replaced, keep := fn(line); keep {
			out = append(out, replaced)
		}
	}
	return strings.Join(out, "\n")
}

func stripEmphasis(text string) string {
	return outsideFences(text, func(line string) (string, bool) {
		return italicPair.ReplaceAllString(boldPair.ReplaceAllString(line, "$1"), "$1"), true
	})
}

func filterCSSLines(text string) string {
	return outsideFences(text, func(line string) (string, bool) { return line, !isCSSClassLine(line) })
}

func deduplicateParagraphs(text string) string {
	seen := map[string]bool{}
	var out []string
	for _, para := range paragraphGap.Split(text, -1) {
		normalized := jsTrim(collapse(para))
		if normalized == "" {
			out = append(out, para)
			continue
		}
		prefix := utf16Prefix(normalized, 60)
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		out = append(out, para)
	}
	return strings.Join(out, "\n\n")
}

func linksSection(links []link) string {
	seen := map[string]bool{}
	var unique []string
	for _, l := range links {
		if l.href == "" || seen[l.href] || strings.HasPrefix(l.href, "#") || strings.HasPrefix(l.href, "javascript:") {
			continue
		}
		seen[l.href] = true
		unique = append(unique, "- "+l.text+": "+l.href)
	}
	if len(unique) == 0 {
		return ""
	}
	return "\n\n## Links\n" + strings.Join(unique, "\n")
}

func structuredSection(items []*value) string {
	if len(items) == 0 {
		return ""
	}
	return "\n\n## Structured Data\n```json\n" + (&value{kind: 'a', vals: items}).stringify(2) + "\n```"
}

func optimize(raw string, meta Metadata, links []link, structured []*value, logos []string) string {
	var header []string
	if meta.URL != "" {
		header = append(header, "> URL: "+meta.URL)
	}
	if meta.Title != "" {
		header = append(header, "> Title: "+meta.Title)
	}
	if meta.Description != "" {
		header = append(header, "> Description: "+meta.Description)
	}
	if meta.Author != "" {
		header = append(header, "> Author: "+meta.Author)
	}
	header = append(header, "> Word count: "+strconv.Itoa(wordCount(raw)))

	text := raw
	if len(logos) > 1 {
		text = strings.Join(logos, ", ") + "\n\n" + text
	}
	text = stripEmphasis(text)
	text = deduplicateParagraphs(text)
	text = strings.Join(mergeStats(strings.Split(text, "\n")), "\n")
	text = trailingBlank.ReplaceAllString(text, "\n")
	text = manyNewlines.ReplaceAllString(text, "\n\n")
	text = strings.ReplaceAll(text, "\t", "  ")
	text = jsTrim(text)
	text = filterCSSLines(text)
	return strings.Join(header, "\n") + "\n\n" + text + linksSection(links) + structuredSection(structured)
}

// Extract converts page HTML fetched from pageURL into Markdown for a model.
func Extract(page, pageURL string) Result {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return Result{Markdown: utf16Prefix(page, 4000), Strategies: []string{"fallback_raw"}}
	}
	var strategies []string
	originalH1 := ""
	if h1 := first(doc, byTag("h1")); h1 != nil {
		originalH1 = jsTrim(text(h1))
	}
	structured := extractStructuredData(doc)
	nextData := extractNextData(doc)
	meta := extractMetadata(doc, pageURL)

	removeNoise(doc)
	md, links, logos := htmlToMarkdown(selectMainContent(doc), pageURL)
	if wordCount(md) < 100 {
		md, links, logos = htmlToMarkdown(body(doc), pageURL)
		strategies = append(strategies, "sparse_recovery")
	} else {
		strategies = append(strategies, "readability_score")
	}
	if originalH1 != "" && !strings.Contains(md, originalH1) {
		md = "# " + originalH1 + "\n\n" + md
	}
	finalWords := wordCount(md)
	if finalWords < 200 {
		if len(structured) > 0 {
			strategies = append(strategies, "data_island_schema")
		}
		if nextData != nil {
			if s := nextData.stringify(2); utf16Len(s) < 8000 {
				md += "\n\n" + s
				strategies = append(strategies, "data_island_next")
			}
		}
	}
	return Result{
		Markdown:   optimize(md, meta, links, structured, logos),
		Metadata:   meta,
		WordCount:  finalWords,
		Strategies: strategies,
	}
}
