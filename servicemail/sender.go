package servicemail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	mail "github.com/wneessen/go-mail"
)

// ErrNotConfigured is returned when no SMTP server is set up.
var ErrNotConfigured = errors.New("servicemail: service email is not configured")

// Mailer sends one rendered message. Apps depend on this, not on SMTP.
type Mailer interface {
	Send(ctx context.Context, to string, m Message) error
}

// Sender delivers mail over SMTP.
type Sender struct{ Cfg Config }

var _ Mailer = Sender{}

// footer follows the Node server: the support address is mentioned when set.
func (s Sender) footer(m Message) string {
	if m.Footer != "" {
		return m.Footer
	}
	if s.Cfg.SupportURL != "" {
		return fmt.Sprintf("This mailbox is used only for %s service notifications. Support: %s", s.Cfg.BrandName, s.Cfg.SupportURL)
	}
	return defaultFooter(s.Cfg.BrandName)
}

// Message builds the MIME message (text and HTML alternatives).
func (s Sender) message(to string, m Message) (*mail.Msg, error) {
	m.BrandName = s.Cfg.BrandName
	m.Footer = s.footer(m)
	for _, v := range []string{to, m.Subject, s.Cfg.From, s.Cfg.ReplyTo} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("servicemail: header values must be a single line")
		}
	}
	msg := mail.NewMsg()
	if err := msg.From(s.Cfg.From); err != nil {
		return nil, fmt.Errorf("servicemail: sender address: %w", err)
	}
	if err := msg.To(to); err != nil {
		return nil, fmt.Errorf("servicemail: recipient address: %w", err)
	}
	if s.Cfg.ReplyTo != "" {
		if err := msg.ReplyTo(s.Cfg.ReplyTo); err != nil {
			return nil, fmt.Errorf("servicemail: reply-to address: %w", err)
		}
	}
	msg.Subject(m.Subject)
	for name, value := range m.Headers {
		if strings.ContainsAny(name+value, "\r\n") {
			return nil, errors.New("servicemail: header values must be a single line")
		}
		msg.SetGenHeader(mail.Header(name), value)
	}
	msg.SetBodyString(mail.TypeTextPlain, RenderText(m))
	msg.AddAlternativeString(mail.TypeTextHTML, RenderHTML(m))
	for _, a := range m.Attachments {
		var opts []mail.FileOption
		if a.ContentType != "" {
			opts = append(opts, mail.WithFileContentType(mail.ContentType(a.ContentType)))
		}
		if err := msg.AttachReader(a.Name, bytes.NewReader(a.Content), opts...); err != nil {
			return nil, fmt.Errorf("servicemail: attachment: %w", err)
		}
	}
	return msg, nil
}

// Send delivers m to the address. It fails with ErrNotConfigured when email is off.
func (s Sender) Send(ctx context.Context, to string, m Message) error {
	if !s.Cfg.Enabled {
		return ErrNotConfigured
	}
	msg, err := s.message(to, m)
	if err != nil {
		return err
	}
	opts := []mail.Option{
		mail.WithPort(s.Cfg.Port),
		mail.WithTimeout(30 * time.Second),
		mail.WithTLSConfig(&tls.Config{ServerName: s.Cfg.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: !s.Cfg.RejectUnauthorized}),
	}
	switch {
	case s.Cfg.Secure:
		opts = append(opts, mail.WithSSL())
	case s.Cfg.RequireTLS:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSOpportunistic))
	}
	if s.Cfg.User != "" || s.Cfg.Pass != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover), mail.WithUsername(s.Cfg.User), mail.WithPassword(s.Cfg.Pass))
	}
	client, err := mail.NewClient(s.Cfg.Host, opts...)
	if err != nil {
		return fmt.Errorf("servicemail: smtp client: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("servicemail: send: %w", err)
	}
	return nil
}
