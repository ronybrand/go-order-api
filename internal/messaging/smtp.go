package messaging

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

const smtpTimeout = 30 * time.Second

// SMTPConfig is the connection to the mail server.
type SMTPConfig struct {
	Host, User, Password, From string
	Port                       int
	// RequireTLS makes STARTTLS mandatory: if the server does not offer it the mail is not sent, instead of
	// going out (and the credentials with it) in clear text. Production sets it.
	RequireTLS bool
}

// SMTPMailer sends mail through an SMTP server with net/smtp.
type SMTPMailer struct {
	cfg SMTPConfig
}

// NewSMTPMailer validates the configuration.
func NewSMTPMailer(cfg SMTPConfig) (*SMTPMailer, error) {
	if cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 {
		return nil, errors.New("smtp host and a valid port are required")
	}
	if addr, err := mail.ParseAddress(cfg.From); err != nil || addr.Address != cfg.From {
		return nil, errors.New("smtp sender must be a bare e-mail address")
	}
	return &SMTPMailer{cfg: cfg}, nil
}

// Send delivers one HTML message.
func (m *SMTPMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	msg, err := buildMessage(m.cfg.From, to, subject, htmlBody)
	if err != nil {
		return fmt.Errorf("%w: %w", err, ErrPermanent)
	}
	address := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	dialer := net.Dialer{Timeout: smtpTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial smtp: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(smtpTimeout))
	client, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close() //nolint:errcheck // best effort after Quit

	secure := false
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
		secure = true
	}
	if m.cfg.RequireTLS && !secure {
		return errors.New("smtp server does not offer STARTTLS and TLS is required")
	}
	if m.cfg.User != "" {
		if err := client.Auth(smtp.PlainAuth("", m.cfg.User, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(m.cfg.From); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}

// buildMessage assembles the RFC 5322 message. Header values with CR or LF are refused: they are how a
// header injection adds a Bcc.
func buildMessage(from, to, subject, htmlBody string) ([]byte, error) {
	for name, value := range map[string]string{"from": from, "to": to, "subject": subject} {
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("invalid %s header", name)
		}
	}
	var b strings.Builder
	for _, header := range [][2]string{
		{"From", from}, {"To", to}, {"Subject", subject},
		{"MIME-Version", "1.0"}, {"Content-Type", "text/html; charset=UTF-8"},
	} {
		b.WriteString(header[0] + ": " + header[1] + "\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(htmlBody, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String()), nil
}
