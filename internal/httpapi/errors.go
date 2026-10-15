// Package httpapi is the HTTP adapter: routing, middleware, JSON mapping and error responses. It holds no
// business rule, it translates between HTTP and the app package.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/ronybrand/go-order-api/internal/apperr"
)

type contextKey int

const requestIDKey contextKey = iota

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// errorResponse is the error body of the whole API: {message, code, params, request_id}.
type errorResponse struct {
	Message   string         `json:"message"`
	Code      string         `json:"code"`
	Params    map[string]any `json:"params"`
	RequestID string         `json:"request_id"`
}

var statusByKind = map[apperr.Kind]int{
	apperr.KindValidation:      http.StatusBadRequest,
	apperr.KindNotFound:        http.StatusNotFound,
	apperr.KindConflict:        http.StatusConflict,
	apperr.KindUnauthenticated: http.StatusUnauthorized,
	apperr.KindForbidden:       http.StatusForbidden,
	apperr.KindTooLarge:        http.StatusRequestEntityTooLarge,
	apperr.KindRateLimited:     http.StatusTooManyRequests,
}

// writeError renders err. An *apperr.Error is shown as is; anything else is an unexpected failure: it is
// logged in full and the client only sees INTERNAL-00, never the underlying message.
func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		err = apperr.TooLarge(maxBytes.Limit)
	}
	status, body := http.StatusInternalServerError, errorResponse{
		Message: "Internal error", Code: apperr.CodeInternal, Params: map[string]any{}, RequestID: requestIDFrom(r.Context()),
	}
	if appErr, ok := apperr.As(err); ok {
		status = statusByKind[appErr.Kind]
		body.Message, body.Code, body.Params = appErr.Message, appErr.Code, appErr.Params
		if body.Params == nil {
			body.Params = map[string]any{}
		}
	} else {
		if logger == nil {
			logger = slog.Default()
		}
		logger.ErrorContext(r.Context(), "unexpected error", "error", err, "request_id", body.RequestID)
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the status line is already out; nothing useful to do on failure
}

// decodeJSON reads exactly one JSON value from the body. Unknown fields, trailing data and wrong types are
// VALIDATION-01; the offending content is never echoed back.
func decodeJSON(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	decoder.UseNumber() // numbers stay exact until the field type decides what they are
	if err := decoder.Decode(into); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return err
		}
		return apperr.InvalidField("body", "request body is not valid for this endpoint")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return err
		}
		return apperr.InvalidField("body", "request body must contain a single JSON value")
	}
	return nil
}
