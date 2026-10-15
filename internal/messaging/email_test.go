package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeMailer struct {
	to, subject, html string
	err               error
}

func (f *fakeMailer) Send(_ context.Context, to, subject, html string) error {
	f.to, f.subject, f.html = to, subject, html
	return f.err
}

func TestEmailNotifier_RendersTheTemplate(t *testing.T) {
	mailer := &fakeMailer{}
	notifier, err := NewEmailNotifier(mailer)
	if err != nil {
		t.Fatal(err)
	}
	e := event()

	err = notifier.Notify(context.Background(), e)

	if err != nil || mailer.to != "ana@example.com" {
		t.Fatalf("to=%q err=%v", mailer.to, err)
	}
	for _, want := range []string{e.OrderID.String(), "Ana", "CONFIRMED", "10.30"} {
		if !strings.Contains(mailer.html, want) {
			t.Errorf("body is missing %q:\n%s", want, mailer.html)
		}
	}
	if !strings.Contains(mailer.subject, "CONFIRMED") {
		t.Errorf("subject = %q", mailer.subject)
	}
}

func TestEmailNotifier_EscapesTheCustomerName(t *testing.T) {
	mailer := &fakeMailer{}
	notifier, _ := NewEmailNotifier(mailer)
	e := event()
	e.CustomerName = `<script>alert(1)</script>`

	if err := notifier.Notify(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(mailer.html, "<script>") {
		t.Errorf("name was not escaped:\n%s", mailer.html)
	}
}

func TestEmailNotifier_RejectsRecipientsThatCouldInjectHeaders(t *testing.T) {
	notifier, _ := NewEmailNotifier(&fakeMailer{})

	for _, email := range []string{"a@example.com\r\nBcc: evil@example.com", "not-an-email", "", "Ana <a@example.com>"} {
		e := event()
		e.CustomerEmail = email

		if err := notifier.Notify(context.Background(), e); !errors.Is(err, ErrPermanent) {
			t.Errorf("%q: err = %v, want ErrPermanent", email, err)
		}
	}
}

func TestEmailNotifier_TransportErrorsAreRetryable(t *testing.T) {
	notifier, _ := NewEmailNotifier(&fakeMailer{err: errors.New("connection refused")})

	err := notifier.Notify(context.Background(), event())

	if err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("err = %v, want a retryable error", err)
	}
}
