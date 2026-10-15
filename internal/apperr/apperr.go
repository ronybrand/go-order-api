// Package apperr defines the error type that crosses every layer of the service.
//
// A domain or application rule that fails returns an *Error carrying a stable, client-facing Code
// (the catalog in the spec: VALIDATION-02, CONFLICT-03...) and a Kind that the HTTP layer maps to a
// status. Anything that is not an *Error is an unexpected failure and is reported as INTERNAL-00
// without leaking its message.
package apperr

import (
	"errors"
	"fmt"
)

// Kind groups errors by the HTTP status family they map to.
type Kind int

// The kinds an Error can have.
const (
	KindValidation Kind = iota + 1
	KindNotFound
	KindConflict
	KindUnauthenticated
	KindForbidden
	KindTooLarge
	KindRateLimited
)

// Error is a failure the client is allowed to see: stable code, readable message, optional parameters.
// Params never carry PII (tax id, e-mail...): they exist for interpolation, not for debugging.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Params  map[string]any
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func newError(kind Kind, code, message string, params map[string]any) *Error {
	return &Error{Kind: kind, Code: code, Message: message, Params: params}
}

// Validation reports a rule or input violation (HTTP 400).
func Validation(code, message string, params map[string]any) *Error {
	return newError(KindValidation, code, message, params)
}

// InvalidField is the common VALIDATION-01 for a single offending field.
func InvalidField(field, message string) *Error {
	return Validation(CodeInvalidRequest, message, map[string]any{"field": field})
}

// NotFound reports a missing resource (HTTP 404).
func NotFound(code, message string, params map[string]any) *Error {
	return newError(KindNotFound, code, message, params)
}

// Conflict reports a state conflict (HTTP 409).
func Conflict(code, message string, params map[string]any) *Error {
	return newError(KindConflict, code, message, params)
}

// Unauthenticated reports a missing or invalid credential (HTTP 401).
func Unauthenticated() *Error {
	return newError(KindUnauthenticated, CodeUnauthenticated, "Authentication required", nil)
}

// Forbidden reports a valid credential without the required role (HTTP 403).
func Forbidden() *Error {
	return newError(KindForbidden, CodeForbidden, "Access denied", nil)
}

// TooLarge reports a request body above the configured limit (HTTP 413).
func TooLarge(maxBytes int64) *Error {
	return newError(KindTooLarge, CodeBodyTooLarge, "Request body too large", map[string]any{"max_bytes": maxBytes})
}

// RateLimited reports an exceeded rate limit (HTTP 429).
func RateLimited() *Error {
	return newError(KindRateLimited, CodeRateLimit, "Rate limit exceeded", map[string]any{})
}

// As extracts the *Error from an error chain.
func As(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// Is reports whether err carries the given code.
func Is(err error, code string) bool {
	appErr, ok := As(err)
	return ok && appErr.Code == code
}
