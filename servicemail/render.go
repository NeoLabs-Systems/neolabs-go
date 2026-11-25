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

// RenderHTML renders the message as the NeoLabs HTML email.
func RenderHTML(m Message) string {
	brand := brandOrDefault(m)
	footer := m.Footer
	if footer == "" {
		footer = defaultFooter(brand)
	}
	action := ""
	if m.ActionURL != "" && m.ActionLabel != "" {
		action = fill(actionTmpl, "sans", fontSans,
			"actionUrl", escapeHTML(m.ActionURL), "actionLabel", escapeHTML(m.ActionLabel))
	}
	return fill(pageTmpl,
		"sans", fontSans, "serif", fontSerif, "mono", fontMono,
		"title", escapeHTML(m.Title), "intro", escapeHTML(m.Intro), "brand", escapeHTML(brand),
		"footer", escapeHTML(footer), "fontLink", fontLink, "darkStyles", darkStyles,
		"action", action, "details", renderDetails(m.Details))
}

// RenderText renders the plain-text alternative.
func RenderText(m Message) string {
	lines := []string{m.Title, "", m.Intro}
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
