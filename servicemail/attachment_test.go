package servicemail

import (
	"bytes"
	"strings"
	"testing"
)

func TestAttachmentsHeadersAndPersonalNote(t *testing.T) {
	sender := Sender{Cfg: Config{From: "app@example.com", BrandName: "App"}}
	msg, err := sender.message("to@example.com", Message{
		Subject: "S", Title: "T", Intro: "I", Lang: "de", Eyebrow: "Request", Fallback: "Open this:",
		ActionURL: "https://example.com", ActionLabel: "Go",
		Paragraphs:  []string{"<second>"},
		Quote:       &Quote{Label: "Note", Text: "one\n<two>"},
		Attachments: []Attachment{{Name: "file (signed).pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.7")}},
		Headers:     map[string]string{"Auto-Submitted": "auto-generated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	if _, err := msg.WriteTo(&raw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Auto-Submitted: auto-generated", "application/pdf", "file (signed).pdf", "JVBERi0xLjc="} {
		if !strings.Contains(raw.String(), want) {
			t.Errorf("message lacks %q", want)
		}
	}
	m := Message{Title: "T", Intro: "I", Lang: "de", Eyebrow: "Request", Quote: &Quote{Text: "<two>"}, Paragraphs: []string{"<second>"}}
	html := RenderHTML(m)
	for _, want := range []string{`<html lang="de">`, ">Request</td>", "&lt;second&gt;", "&lt;two&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	if !strings.Contains(RenderText(m), "> <two>") {
		t.Error("text lacks the quoted note")
	}
	if _, err := sender.message("to@example.com", Message{Subject: "S", Headers: map[string]string{"X": "a\r\nBcc: x@y.z"}}); err == nil {
		t.Error("header injection accepted")
	}
}
