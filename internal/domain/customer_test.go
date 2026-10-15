package domain_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ronybrand/go-order-api/internal/apperr"
	"github.com/ronybrand/go-order-api/internal/domain"
)

var now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func validInput() domain.CustomerInput {
	return domain.CustomerInput{Name: "Ada Lovelace", TaxID: "AB123456", Email: "ada@example.com"}
}

func TestNewCustomer_AcceptsAValidInput(t *testing.T) {
	c, err := domain.NewCustomer(validInput(), "admin-1", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.CreatedBy != "admin-1" || c.UpdatedBy != "admin-1" || !c.CreatedAt.Equal(now) || c.Version != 1 {
		t.Errorf("audit fields not filled: %+v", c)
	}
}

func TestNewCustomer_StoresTheTaxIDInCanonicalForm(t *testing.T) {
	tests := map[string]string{
		"123.456-78": "12345678",
		"ab-12.cd/9": "AB12CD9",
		"abcde":      "ABCDE",
		"AB123456":   "AB123456",
	}
	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			in := validInput()
			in.TaxID = raw
			c, err := domain.NewCustomer(in, "a", now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.TaxID != want {
				t.Errorf("TaxID = %q, want %q", c.TaxID, want)
			}
		})
	}
}

func TestNewCustomer_FormattingDifferencesAreTheSameDocument(t *testing.T) {
	a, b := validInput(), validInput()
	a.TaxID, b.TaxID = "123.456-78", "12345678"
	first, _ := domain.NewCustomer(a, "a", now)
	second, _ := domain.NewCustomer(b, "a", now)
	if first.TaxID != second.TaxID {
		t.Errorf("%q != %q", first.TaxID, second.TaxID)
	}
}

func TestNewCustomer_RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		mod   func(*domain.CustomerInput)
		field string
	}{
		{"blank name", func(in *domain.CustomerInput) { in.Name = "  \t" }, "name"},
		{"name above the column", func(in *domain.CustomerInput) { in.Name = strings.Repeat("a", 201) }, "name"},
		{"tax id too short", func(in *domain.CustomerInput) { in.TaxID = "1234" }, "tax_id"},
		{"tax id too long", func(in *domain.CustomerInput) { in.TaxID = strings.Repeat("1", 21) }, "tax_id"},
		{"tax id with a trailing newline", func(in *domain.CustomerInput) { in.TaxID = "12345678\n" }, "tax_id"},
		{"tax id with only punctuation", func(in *domain.CustomerInput) { in.TaxID = "....." }, "tax_id"},
		{"tax id with fewer than 5 alphanumerics", func(in *domain.CustomerInput) { in.TaxID = "A.B-C" }, "tax_id"},
		{"lower-case passport", func(in *domain.CustomerInput) { in.PassportNumber = ptr("ab123456") }, "passport_number"},
		{"short passport", func(in *domain.CustomerInput) { in.PassportNumber = ptr("AB123") }, "passport_number"},
		{"empty email", func(in *domain.CustomerInput) { in.Email = "" }, "email"},
		{"email without domain", func(in *domain.CustomerInput) { in.Email = "ada" }, "email"},
		{"email with a display name", func(in *domain.CustomerInput) { in.Email = "Ada <ada@example.com>" }, "email"},
		{"email list", func(in *domain.CustomerInput) { in.Email = "a@example.com, b@example.com" }, "email"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.mod(&in)
			_, err := domain.NewCustomer(in, "a", now)
			appErr, ok := apperr.As(err)
			if !ok || appErr.Code != apperr.CodeInvalidRequest || appErr.Params["field"] != tt.field {
				t.Errorf("err = %v, want VALIDATION-01 on %q", err, tt.field)
			}
		})
	}
}

func TestNewCustomer_PassportIsOptional(t *testing.T) {
	for _, passport := range []*string{nil, ptr("")} {
		in := validInput()
		in.PassportNumber = passport
		c, err := domain.NewCustomer(in, "a", now)
		if err != nil || c.PassportNumber != nil {
			t.Errorf("passport %v: err=%v stored=%v", passport, err, c)
		}
	}
	in := validInput()
	in.PassportNumber = ptr("AB123456")
	if c, err := domain.NewCustomer(in, "a", now); err != nil || *c.PassportNumber != "AB123456" {
		t.Errorf("valid passport rejected: %v", err)
	}
}

func TestCustomer_ReplaceKeepsTheOriginalCreationAudit(t *testing.T) {
	c, _ := domain.NewCustomer(validInput(), "creator", now)
	later := now.Add(time.Hour)
	in := validInput()
	in.Name = "New Name"

	if err := c.Replace(in, "editor", later); err != nil {
		t.Fatal(err)
	}
	if c.Name != "New Name" || c.CreatedBy != "creator" || c.UpdatedBy != "editor" || !c.UpdatedAt.Equal(later) {
		t.Errorf("audit after replace: %+v", c)
	}
}

func TestCustomer_ReplaceDoesNotTouchTheCustomerWhenTheInputIsInvalid(t *testing.T) {
	c, _ := domain.NewCustomer(validInput(), "creator", now)
	in := validInput()
	in.Name = ""

	if err := c.Replace(in, "editor", now.Add(time.Hour)); err == nil {
		t.Fatal("expected an error")
	}
	if c.Name != "Ada Lovelace" || c.UpdatedBy != "creator" {
		t.Errorf("customer changed despite the error: %+v", c)
	}
}

func TestCustomer_LogValueExposesOnlyTheID(t *testing.T) {
	c, _ := domain.NewCustomer(validInput(), "a", now)
	var out bytes.Buffer
	slog.New(slog.NewTextHandler(&out, nil)).Info("loaded", "customer", c)
	rendered := out.String()
	for _, pii := range []string{"AB123456", "ada@example.com", "Ada Lovelace"} {
		if strings.Contains(rendered, pii) {
			t.Errorf("log value leaks %q: %s", pii, rendered)
		}
	}
}
