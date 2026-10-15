// Package domain holds the entities and business rules of the order domain. It depends on nothing but
// the standard library, decimal arithmetic and apperr: no HTTP, no SQL, no broker.
package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ronybrand/go-order-api/internal/apperr"
)

// Field limits shared by validation and the database schema (migrations/0001): the database refuses
// what the domain would refuse, and the numbers cannot drift apart silently.
const (
	MaxNameLength          = 200
	MaxEmailLength         = 320
	MaxTaxIDLength         = 20
	MaxPassportLength      = 9
	MaxActorLength         = 200
	minTaxIDSignificantLen = 5
)

var (
	// Go's RE2 "$" only matches at the end of the text (no trailing-newline surprise).
	taxIDPattern    = regexp.MustCompile(`^[A-Za-z0-9./-]{5,20}$`)
	passportPattern = regexp.MustCompile(`^[A-Z0-9]{6,9}$`)
)

// Customer is a registered customer. TaxID, PassportNumber and Email are PII: they are never logged and
// never returned by the API (see Customer.LogValue).
type Customer struct {
	ID             uuid.UUID
	Name           string
	TaxID          string
	PassportNumber *string
	Email          string
	Version        int64

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
	UpdatedBy string
	DeletedAt *time.Time
	DeletedBy *string
}

// CustomerInput is the data a client supplies to create or replace a customer.
type CustomerInput struct {
	Name           string
	TaxID          string
	PassportNumber *string
	Email          string
}

// NewCustomer validates the input and builds a customer. now and actor feed the audit fields.
func NewCustomer(in CustomerInput, actor string, now time.Time) (*Customer, error) {
	c := &Customer{CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor, Version: 1}
	if err := c.Replace(in, actor, now); err != nil {
		return nil, err
	}
	return c, nil
}

// Replace validates the input and overwrites the customer's data (PUT semantics).
func (c *Customer) Replace(in CustomerInput, actor string, now time.Time) error {
	valid, err := in.validated()
	if err != nil {
		return err
	}
	c.Name, c.TaxID, c.PassportNumber, c.Email = valid.Name, valid.TaxID, valid.PassportNumber, valid.Email
	c.UpdatedAt, c.UpdatedBy = now, actor
	return nil
}

func (in CustomerInput) validated() (CustomerInput, error) {
	name := in.Name
	switch {
	case strings.TrimSpace(name) == "":
		return CustomerInput{}, apperr.InvalidField("name", "name must not be blank")
	case utf8.RuneCountInString(name) > MaxNameLength:
		return CustomerInput{}, apperr.InvalidField("name", "name is too long")
	}

	if !taxIDPattern.MatchString(in.TaxID) {
		return CustomerInput{}, apperr.InvalidField("tax_id", "tax_id has an invalid format")
	}
	taxID := CanonicalTaxID(in.TaxID)
	if len(taxID) < minTaxIDSignificantLen {
		return CustomerInput{}, apperr.InvalidField("tax_id", "tax_id must contain at least 5 letters or digits")
	}

	var passport *string
	if in.PassportNumber != nil && *in.PassportNumber != "" {
		if !passportPattern.MatchString(*in.PassportNumber) {
			return CustomerInput{}, apperr.InvalidField("passport_number", "passport_number has an invalid format")
		}
		p := *in.PassportNumber
		passport = &p
	}

	email, err := validEmail(in.Email)
	if err != nil {
		return CustomerInput{}, err
	}
	return CustomerInput{Name: name, TaxID: taxID, PassportNumber: passport, Email: email}, nil
}

// CanonicalTaxID keeps only letters and digits, upper-cased: "123.456-78" and "12345678" are the same
// document, so the stored value (and the uniqueness check) always uses this form.
func CanonicalTaxID(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

func validEmail(raw string) (string, error) {
	if raw == "" || len(raw) > MaxEmailLength {
		return "", apperr.InvalidField("email", "email is invalid")
	}
	// ParseAddress also accepts "Name <a@b>" and comments; only a bare address is a valid e-mail here.
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address != raw || addr.Name != "" {
		return "", apperr.InvalidField("email", "email is invalid")
	}
	return raw, nil
}
