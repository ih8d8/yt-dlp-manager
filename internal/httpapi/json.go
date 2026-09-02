// Package httpapi exposes the manager to browsers over same-origin HTTP+SSE.
// It is an adapter: all state lives in the manager and config packages, and
// nothing here talks to the Unix IPC socket or the TUI.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBodyBytes bounds every ordinary API JSON body.
const MaxBodyBytes = 1 << 20

// Error codes are stable strings the frontend can branch on.
const (
	codeInvalidJSON      = "invalid_json"
	codeInvalidURL       = "invalid_url"
	codeDuplicateURL     = "duplicate_url"
	codeInvalidOptions   = "invalid_options"
	codeInvalidState     = "invalid_state"
	codeNotFound         = "not_found"
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeRateLimited      = "rate_limited"
	codePasswordChange   = "password_change_required"
	codeTooLarge         = "too_large"
	codeMethodNotAllowed = "method_not_allowed"
	codeInternal         = "internal"
)

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	reqID := requestIDFromCtx(r.Context())
	body := errorEnvelope{Error: apiError{Code: code, Message: message, RequestID: reqID}}
	writeJSON(w, status, body)
}

func requestIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// API responses carry session state, CSRF tokens, filesystem paths and
	// download history. Cookie authentication does not automatically prevent a
	// shared reverse-proxy cache from storing a GET response, so make the safe
	// policy explicit for every JSON response (including errors).
	w.Header().Set("Cache-Control", "no-store")
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

var errBodyTooLarge = errors.New("request body too large")

// decodeJSON strictly reads one bounded JSON body: content type required,
// size limited during accumulation, unknown fields rejected, and trailing
// values rejected. Callers that decode into a struct additionally reject
// non-object bodies by construction of the destination type.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	ct := r.Header.Get("Content-Type")
	mt := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	if mt != "application/json" {
		writeError(w, r, http.StatusUnsupportedMediaType, codeInvalidJSON,
			"content type must be application/json")
		return fmt.Errorf("bad content type %q", mt)
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidJSON, "unreadable request body")
		return err
	}
	if int64(len(data)) > maxBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, codeTooLarge,
			fmt.Sprintf("request body exceeds %d bytes", maxBytes))
		return errBodyTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidJSON, "malformed JSON body")
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, codeInvalidJSON, "trailing data after JSON object")
		return errors.New("trailing JSON data")
	}
	return nil
}
