package messaging

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/mail"
	"strings"

	"github.com/ronybrand/go-order-api/internal/domain"
)

// Mailer sends one HTML e-mail. The SMTP implementation is in smtp.go.
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}

// html/template escapes every interpolated value for its HTML context, so a customer named
// "<script>..." cannot inject markup into the message.
var statusEmail = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<body style="font-family: sans-serif">
  <p>Hello, {{.CustomerName}}.</p>
  <p>Your order <strong>{{.OrderID}}</strong> changed from <strong>{{.OldStatus}}</strong> to <strong>{{.NewStatus}}</strong>.</p>
  <p>Total: {{.Total}}</p>
  <p>Updated at {{.ChangedAt}}.</p>
</body>
</html>
`))

// EmailNotifier renders the status e-mail and hands it to a Mailer.
type EmailNotifier struct {
	mailer Mailer
}

// NewEmailNotifier builds the notifier.
func NewEmailNotifier(mailer Mailer) (*EmailNotifier, error) {
	if mailer == nil {
		return nil, fmt.Errorf("mailer is required")
	}
	return &EmailNotifier{mailer: mailer}, nil
}

// Notify validates the recipient (a bad one never becomes valid by retrying: ErrPermanent) and sends the mail.
func (n *EmailNotifier) Notify(ctx context.Context, event domain.OrderStatusChanged) error {
	if !validRecipient(event.CustomerEmail) {
		return fmt.Errorf("invalid recipient: %w", ErrPermanent)
	}
	var body bytes.Buffer
	err := statusEmail.Execute(&body, map[string]any{
		"CustomerName": event.CustomerName,
		"OrderID":      event.OrderID.String(),
		"OldStatus":    string(event.OldStatus),
		"NewStatus":    string(event.NewStatus),
		"Total":        event.TotalAmount.StringFixed(2),
		"ChangedAt":    event.ChangedAt.UTC().Format("2006-01-02 15:04 MST"),
	})
	if err != nil {
		return fmt.Errorf("render e-mail: %w: %w", err, ErrPermanent)
	}
	subject := "Order " + string(event.NewStatus)
	if err := n.mailer.Send(ctx, event.CustomerEmail, subject, body.String()); err != nil {
		return fmt.Errorf("send e-mail: %w", err)
	}
	return nil
}

// validRecipient accepts only a bare address: control characters would allow header injection, and a
// display name has no place in a stored e-mail.
func validRecipient(email string) bool {
	if email == "" || strings.ContainsAny(email, "\r\n") {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email && addr.Name == ""
}
