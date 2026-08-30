package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// expectedAPIPaths must stay in sync with routes(): every registered API
// path must appear in the OpenAPI document.
var expectedAPIPaths = []string{
	"/healthz",
	"/readyz",
	"/api/v1/session",
	"/api/v1/session/password",
	"/api/v1/downloads",
	"/api/v1/downloads/{id}",
	"/api/v1/downloads/{id}/thumbnail",
	"/api/v1/downloads/actions",
	"/api/v1/downloads/clear",
	"/api/v1/events",
	"/api/v1/settings",
	"/api/v1/settings/yt-dlp",
	"/api/v1/settings/yt-dlp/managed",
	"/api/v1/system",
	"/api/v1/openapi.json",
}

func fetchOpenAPIDoc(t *testing.T, s *Server) (int, string, string, map[string]any) {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	ct := rec.Header().Get("Content-Type")
	var doc map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("openapi.json is not valid JSON: %v", err)
		}
	}
	return rec.Code, ct, rec.Body.String(), doc
}

// TestOpenAPIValidAndComplete: the document parses, declares OpenAPI 3.1,
// is served as application/json, and covers every registered API path.
func TestOpenAPIValidAndComplete(t *testing.T) {
	d := testDeps(t)
	s := testServer(t, d)

	code, ct, recBody, doc := fetchOpenAPIDoc(t, s)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if ct != "application/json; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	if v, ok := doc["openapi"].(string); !ok || len(v) < 4 || v[:4] != "3.1." {
		t.Errorf("openapi version = %v, want 3.1.x", doc["openapi"])
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("paths object missing")
	}
	for _, p := range expectedAPIPaths {
		if _, ok := paths[p]; !ok {
			t.Errorf("path %q missing from OpenAPI document", p)
		}
	}
	if len(paths) != len(expectedAPIPaths) {
		t.Errorf("document has %d paths, want exactly %d (stale extra paths?)", len(paths), len(expectedAPIPaths))
	}
	// Methods per path must match the registered routes.
	wantMethods := map[string][]string{
		"/healthz":                         {"get"},
		"/readyz":                          {"get"},
		"/api/v1/session":                  {"get", "post", "delete"},
		"/api/v1/session/password":         {"put"},
		"/api/v1/downloads":                {"get", "post"},
		"/api/v1/downloads/{id}":           {"get"},
		"/api/v1/downloads/{id}/thumbnail": {"get"},
		"/api/v1/downloads/actions":        {"post"},
		"/api/v1/downloads/clear":          {"post"},
		"/api/v1/events":                   {"get"},
		"/api/v1/settings":                 {"get", "put"},
		"/api/v1/settings/yt-dlp":          {"get"},
		"/api/v1/settings/yt-dlp/managed":  {"put"},
		"/api/v1/system":                   {"get"},
		"/api/v1/openapi.json":             {"get"},
	}
	for p, methods := range wantMethods {
		pathItem, ok := paths[p].(map[string]any)
		if !ok {
			continue
		}
		if got := len(pathItem); got != len(methods) {
			t.Errorf("path %q documents %d methods, want %d", p, got, len(methods))
		}
		for _, m := range methods {
			if _, ok := pathItem[m]; !ok {
				t.Errorf("path %q missing method %q", p, m)
			}
		}
	}

	// Security + CSRF contract spot checks.
	components, _ := doc["components"].(map[string]any)
	schemes, _ := components["securitySchemes"].(map[string]any)
	cookie, _ := schemes["cookieAuth"].(map[string]any)
	if cookie == nil || cookie["name"] != "ytdlp_session" || cookie["in"] != "cookie" {
		t.Error("cookieAuth security scheme must name the ytdlp_session cookie")
	}
	actionsItem := paths["/api/v1/downloads/actions"].(map[string]any)["post"].(map[string]any)
	params, _ := actionsItem["parameters"].([]any)
	foundCSRF := false
	for _, p := range params {
		if pm, ok := p.(map[string]any); ok && pm["name"] == "X-CSRF-Token" {
			foundCSRF = true
		}
	}
	if !foundCSRF {
		t.Error("protected mutation must document the X-CSRF-Token header")
	}

	// No secrets or implementation internals in the served document.
	for _, forbidden := range []string{"correct-horse", "password-file", "sessions.key", "0123456789abcdef"} {
		if strings.Contains(recBody, forbidden) {
			t.Errorf("document contains forbidden string %q", forbidden)
		}
	}
}

// TestOpenAPIRequiresAuth: the docs endpoint is not available to
// unauthenticated callers when authentication is enabled.
func TestOpenAPIRequiresAuth(t *testing.T) {
	d, pw := authedDeps(t)
	s := testServer(t, d)

	rec := do(t, s, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}

	cookie, _, code := login(t, s, pw)
	if code != http.StatusOK || cookie == nil {
		t.Fatalf("login failed: %d", code)
	}
	rec = do(t, s, http.MethodGet, "/api/v1/openapi.json", nil,
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := doc["paths"]; !ok {
		t.Error("paths missing")
	}
}
