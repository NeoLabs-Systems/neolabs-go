// Package servicemail sends account emails (confirmation, password reset,
// sign-in and change notices) over SMTP and renders them in the NeoLabs style.
// Rendering, configuration names and token rules match the Node servers.
package servicemail

import (
	"strings"
)

// Detail is one label/value row of a message.
type Detail struct {
	Label string
	Value string
}

// Message is one email before rendering.
type Message struct {
	Subject     string
	Title       string
	Intro       string
	ActionURL   string
	ActionLabel string
	Details     []Detail
	Footer      string
	BrandName   string

	Lang        string   // document language of the HTML part; "en" when empty
	Eyebrow     string   // small label above the title; "Account" when empty
	Fallback    string   // line above the plain link under the button
	Paragraphs  []string // further text after the intro
	Quote       *Quote   // words of a person, set apart from the app's own
	Attachments []Attachment
	Headers     map[string]string // extra message headers, such as Auto-Submitted
}

// Quote is a personal note shown apart from the app's text.
type Quote struct {
	Label string
	Text  string
}

// Attachment is a file sent with a message, always given as bytes.
type Attachment struct {
	Name        string
	ContentType string
	Content     []byte
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

// fill replaces {{key}} markers; values are inserted as they are.
func fill(t string, kv ...string) string {
	pairs := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, "{{"+kv[i]+"}}", kv[i+1])
	}
	return strings.NewReplacer(pairs...).Replace(t)
}

func defaultFooter(brand string) string {
	return "This mailbox is used only for " + brand + " service notifications."
}

func brandOrDefault(m Message) string {
	if m.BrandName == "" {
		return "NeoAgent"
	}
	return m.BrandName
}

func renderDetails(details []Detail) string {
	if len(details) == 0 {
		return ""
	}
	var rows strings.Builder
	for i, d := range details {
		border := ""
		if i > 0 {
			border = "border-top:1px solid #e6e2d6;"
		}
		rows.WriteString(fill(detailRowTmpl, "mono", fontMono, "border", border, "label", escapeHTML(d.Label), "value", escapeHTML(d.Value)))
	}
	return fill(detailsTableTmpl, "rows", rows.String())
}

const defaultFallback = "If the button does not work, open this link:"

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func renderExtra(m Message) string {
	var b strings.Builder
	for _, p := range m.Paragraphs {
		b.WriteString(fill(paragraphTmpl, "text", escapeHTML(p)))
	}
	if m.Quote != nil && m.Quote.Text != "" {
		label := ""
		if m.Quote.Label != "" {
			label = fill(quoteLabelTmpl, "mono", fontMono, "label", escapeHTML(m.Quote.Label))
		}
		b.WriteString(fill(quoteTmpl, "label", label, "text", escapeHTML(m.Quote.Text)))
	}
	return b.String()
}

// RenderHTML renders the message as the NeoLabs HTML email.
func RenderHTML(m Message) string {
	brand := brandOrDefault(m)
	footer := m.Footer
	if footer == "" {
		footer = defaultFooter(brand)
	}
	action := ""
	if m.ActionURL != "" && m.ActionLabel != "" {
		action = fill(actionTmpl, "sans", fontSans, "fallback", escapeHTML(orDefault(m.Fallback, defaultFallback)),
			"actionUrl", escapeHTML(m.ActionURL), "actionLabel", escapeHTML(m.ActionLabel))
	}
	styles := darkStyles
	if m.Quote != nil && m.Quote.Text != "" {
		styles += quoteDarkStyle
	}
	return fill(pageTmpl,
		"lang", escapeHTML(orDefault(m.Lang, "en")), "eyebrow", escapeHTML(orDefault(m.Eyebrow, "Account")), "extra", renderExtra(m),
		"sans", fontSans, "serif", fontSerif, "mono", fontMono,
		"title", escapeHTML(m.Title), "intro", escapeHTML(m.Intro), "brand", escapeHTML(brand),
		"footer", escapeHTML(footer), "fontLink", fontLink, "darkStyles", styles,
		"action", action, "details", renderDetails(m.Details))
}

// RenderText renders the plain-text alternative.
func RenderText(m Message) string {
	lines := []string{m.Title, "", m.Intro}
	for _, p := range m.Paragraphs {
		lines = append(lines, "", p)
	}
	if m.Quote != nil && m.Quote.Text != "" {
		lines = append(lines, "")
		if m.Quote.Label != "" {
			lines = append(lines, m.Quote.Label+":")
		}
		for _, line := range strings.Split(m.Quote.Text, "\n") {
			lines = append(lines, "> "+line)
		}
	}
	if m.ActionURL != "" && m.ActionLabel != "" {
		lines = append(lines, "", m.ActionLabel+": "+m.ActionURL)
	}
	if len(m.Details) > 0 {
		lines = append(lines, "")
		for _, d := range m.Details {
			lines = append(lines, d.Label+": "+d.Value)
		}
	}
	footer := m.Footer
	if footer == "" {
		footer = defaultFooter("NeoAgent")
	}
	lines = append(lines, "", footer)
	return strings.Join(lines, "\n")
}
