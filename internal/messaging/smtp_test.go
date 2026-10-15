package messaging

import (
	"strings"
	"testing"
)

func TestBuildMessage_HasTheRequiredHeadersAndCRLFLineEndings(t *testing.T) {
	msg, err := buildMessage("orders@example.com", "ana@example.com", "Order CONFIRMED", "<p>hi</p>")

	if err != nil {
		t.Fatal(err)
	}
	text := string(msg)
	for _, want := range []string{
		"From: orders@example.com\r\n", "To: ana@example.com\r\n", "Subject: Order CONFIRMED\r\n",
		"MIME-Version: 1.0\r\n", "Content-Type: text/html; charset=UTF-8\r\n", "\r\n\r\n<p>hi</p>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("message is missing %q:\n%q", want, text)
		}
	}
}

func TestBuildMessage_RefusesHeaderInjection(t *testing.T) {
	cases := map[string][3]string{
		"subject": {"orders@example.com", "ana@example.com", "hi\r\nBcc: evil@example.com"},
		"to":      {"orders@example.com", "ana@example.com\r\nBcc: evil@example.com", "hi"},
		"from":    {"orders@example.com\nX: y", "ana@example.com", "hi"},
	}
	for name, c := range cases {
		if _, err := buildMessage(c[0], c[1], c[2], "body"); err == nil {
			t.Errorf("%s: header injection accepted", name)
		}
	}
}

func TestNewSMTPMailer_RequiresSTARTTLSInProduction(t *testing.T) {
	if _, err := NewSMTPMailer(SMTPConfig{Host: "smtp.example.com", Port: 587, From: "o@example.com", RequireTLS: true}); err != nil {
		t.Errorf("tls required with default settings: %v", err)
	}
	if _, err := NewSMTPMailer(SMTPConfig{Host: "", Port: 587, From: "o@example.com"}); err == nil {
		t.Error("empty host accepted")
	}
	if _, err := NewSMTPMailer(SMTPConfig{Host: "h", Port: 587, From: "not an address"}); err == nil {
		t.Error("invalid sender accepted")
	}
}
