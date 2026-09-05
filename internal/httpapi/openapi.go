package httpapi

import (
	"net/http"

	"yt-dlp-manager/internal/ipc"
)

// handleOpenAPI serves GET /api/v1/openapi.json: a hand-maintained OpenAPI
// 3.1 document compiled into the binary. No CDN, no network fetches, no
// implementation paths or secrets. The document is the contract the built-in
// API Explorer renders.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, openapiDocument())
}

// --- small builders to keep the document readable ---

func oaOperation(summary, description, tag string, protected bool) map[string]any {
	op := map[string]any{
		"summary":     summary,
		"description": description,
		"tags":        []string{tag},
		"responses":   map[string]any{},
	}
	if protected {
		op["security"] = []any{map[string]any{"cookieAuth": []string{}}}
	}
	return op
}

func oaJSONResponse(description, schemaRef string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{"$ref": schemaRef},
			},
		},
	}
}

func oaTextResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"text/plain": map[string]any{"schema": map[string]any{"type": "string"}},
		},
	}
}

func oaErrorResponses() map[string]any {
	return map[string]any{
		"400": oaJSONResponse("malformed request body", "#/components/schemas/Error"),
		"401": oaJSONResponse("not authenticated", "#/components/schemas/Error"),
		"403": oaJSONResponse("missing or invalid CSRF token / cross-origin request", "#/components/schemas/Error"),
		"404": oaJSONResponse("not found", "#/components/schemas/Error"),
		"413": oaJSONResponse("request body too large", "#/components/schemas/Error"),
		"422": oaJSONResponse("validation failed or invalid state", "#/components/schemas/Error"),
		"500": oaJSONResponse("unexpected server error", "#/components/schemas/Error"),
	}
}

func oaBody(schemaRef string, example any) map[string]any {
	media := map[string]any{"schema": map[string]any{"$ref": schemaRef}}
	if example != nil {
		media["example"] = example
	}
	return map[string]any{
		"required": true,
		"content": map[string]any{
			"application/json": media,
		},
	}
}

// oaMutation notes the CSRF requirement on a protected mutating operation.
func oaMutation(op map[string]any, csrfNote string) map[string]any {
	existing, _ := op["parameters"].([]any)
	op["parameters"] = append(existing, map[string]any{
		"name":        "X-CSRF-Token",
		"in":          "header",
		"required":    true,
		"description": csrfNote,
		"schema":      map[string]any{"type": "string"},
	})
	return op
}

func oaPathIDParam() map[string]any {
	return map[string]any{
		"name":        "id",
		"in":          "path",
		"required":    true,
		"description": "download item id",
		"schema":      map[string]any{"type": "string", "maxLength": 64},
	}
}

func openapiDocument() map[string]any {
	downloadRef := "#/components/schemas/Download"
	const (
		protected = true
		public    = false
	)

	getDownloads := oaOperation("List downloads", "Current snapshot of all items, sorted active first. Optional state and case-insensitive title/URL search filters.", "Downloads", protected)
	getDownloads["parameters"] = []any{
		map[string]any{
			"name": "state", "in": "query", "required": false,
			"description": "filter by state", "schema": map[string]any{
				"type": "string", "enum": []string{"queued", "downloading", "paused", "completed", "failed", "deleted"},
			},
		},
		map[string]any{
			"name": "search", "in": "query", "required": false,
			"description": "case-insensitive substring match on title or URL",
			"schema":      map[string]any{"type": "string", "maxLength": 4096},
		},
	}
	getDownloads["responses"] = map[string]any{
		"200": oaJSONResponse("download list", "#/components/schemas/DownloadList"),
	}

	addDownloads := oaMutation(oaOperation(
		"Add download",
		"Queue one video or playlist URL. The URL must be http(s), at most 4096 bytes, without control characters or a leading dash. A URL an existing queued, downloading, paused or completed entry already holds is rejected with 409 duplicate_url rather than queued twice, whatever options accompany it: yt-dlp's output template decides the filename and rarely varies by format, so two entries for one URL would resolve to one file and the second would overwrite or skip the first. To fetch another quality, give the output template a format-distinguishing field (%(format_id)s, %(height)s) so the two cannot collide, or remove the existing entry AND move its file \u2014 removal keeps the media on disk, and yt-dlp would otherwise find it and skip the new download. Failed and deleted entries do not block a re-add. Options are per-download overrides passed to yt-dlp on the command line, so they beat both the configuration file and the global downloads.extra_args for this item only, and anything left unset still comes from them; they are stored with the item, so a retry or a restart downloads what was originally asked for. Every option except extra_args is a closed set of structured choices; extra_args is yt-dlp command line text constrained by an allowlist (see the DownloadOptions schema). With start_now the item jumps the queue and may run beyond max_concurrent, up to a hard ceiling of max_concurrent + 8 concurrent downloads; past that it stays queued until a slot frees. If forcing fails the item stays queued and the response reports the partial result.",
		"Downloads", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	addDownloads["requestBody"] = oaBody("#/components/schemas/AddDownloadRequest", map[string]any{
		"url": "https://www.youtube.com/watch?v=...", "start_now": false,
		"options": map[string]any{"preset": "1080p", "merge_container": "mp4"},
	})
	addDownloads["responses"] = map[string]any{
		"201": oaJSONResponse("queued", "#/components/schemas/AddDownloadResponse"),
		"409": oaJSONResponse("a live entry already holds this URL", "#/components/schemas/Error"),
		"422": oaJSONResponse("invalid URL, unusable options, or an entry too large to store", "#/components/schemas/Error"),
	}

	getDownload := oaOperation("Get download", "One item with full details including recorded file paths and bounded failure text.", "Downloads", protected)
	getDownload["parameters"] = []any{oaPathIDParam()}
	getDownload["responses"] = map[string]any{
		"200": oaJSONResponse("the item", "#/components/schemas/DownloadEnvelope"),
		"404": oaJSONResponse("no such download", "#/components/schemas/Error"),
	}

	thumbnail := oaOperation("Get thumbnail", "Proxied thumbnail image for an item (fetched once from the probed https URL, cached under the state directory). Served as image/jpeg, image/png or image/webp only.", "Downloads", protected)
	thumbnail["parameters"] = []any{oaPathIDParam()}
	thumbnail["responses"] = map[string]any{
		"200": map[string]any{
			"description": "thumbnail image",
			"content": map[string]any{
				"image/jpeg": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
				"image/png":  map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
				"image/webp": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
			},
		},
		"404": oaJSONResponse("no thumbnail available", "#/components/schemas/Error"),
		"502": oaJSONResponse("origin fetch failed", "#/components/schemas/Error"),
	}

	actions := oaMutation(oaOperation(
		"Batch actions",
		"Apply one action to up to 500 ids and return a per-id result so one stale selection never hides other successes. Actions: pause, resume, start_now, remove, retry. A resume/retry/start_now of a failed or deleted row whose URL another live entry has since claimed fails that id with duplicate_url rather than starting a second run against the same output file. Nothing here deletes media: removing a row clears only yt-dlp's own scratch files (.part/.ytdl/fragments) and always keeps the finished output.",
		"Downloads", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	actions["requestBody"] = oaBody("#/components/schemas/ActionsRequest", map[string]any{
		"action": "pause", "ids": []string{"id1", "id2"},
	})
	actions["responses"] = map[string]any{
		"200": oaJSONResponse("per-id results (overall 200 even when some ids fail)", "#/components/schemas/ActionsResponse"),
		"422": oaJSONResponse("unknown action or empty/oversized id list", "#/components/schemas/Error"),
	}

	formats := oaMutation(oaOperation(
		"List formats",
		"Report the video and audio streams one URL offers, so a caller can pick a specific format instead of relying on the configured default. Playlists are not walked: the first entry's formats are returned. Bounded by a concurrency ceiling, a 20s end-to-end timeout (including the wait for a free slot, kept under the server's write timeout so the timeout can actually be reported) and a short server-side cache; storyboard pseudo-formats are omitted, and any format id that would be rejected by POST /api/v1/downloads is never offered. It is a POST because it spawns a yt-dlp process, which keeps it behind the same CSRF check as every other mutation.",
		"Downloads", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	formats["requestBody"] = oaBody("#/components/schemas/FormatsRequest", map[string]any{
		"url": "https://www.youtube.com/watch?v=...",
	})
	formats["responses"] = map[string]any{
		"200": oaJSONResponse("available formats", "#/components/schemas/FormatsResponse"),
		"422": oaJSONResponse("invalid URL", "#/components/schemas/Error"),
		"501": oaJSONResponse("this server cannot list formats", "#/components/schemas/Error"),
		"502": oaJSONResponse("the extractor could not read this URL", "#/components/schemas/Error"),
	}

	clear := oaMutation(oaOperation(
		"Clear queue",
		"Bulk queue action. Scopes \"completed\", \"failed\" and \"deleted\" each remove one terminal state; \"finished\" removes all three. scope \"all\" additionally stops active downloads through the normal cancellation path and removes every queue/history row. Downloaded files are never deleted by any scope — only yt-dlp's own scratch for downloads that were still running. The scope is a server-side state filter, so it clears what is terminal when the request lands, not what the caller last saw.",
		"Downloads", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	clear["requestBody"] = oaBody("#/components/schemas/ClearRequest", map[string]any{"scope": "finished"})
	clear["responses"] = map[string]any{
		"200": oaJSONResponse("removed count", "#/components/schemas/ClearResponse"),
		"422": oaJSONResponse("unknown scope", "#/components/schemas/Error"),
	}

	events := oaOperation("Event stream", "Server-Sent Events stream. The first event is always a full snapshot; later events are named download (upsert), removed (final removal) and stats (periodic aggregate, doubles as heartbeat). Reconnects receive a fresh snapshot, never a replay.", "Events", protected)
	events["responses"] = map[string]any{
		"200": map[string]any{
			"description": "text/event-stream",
			"content": map[string]any{
				"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}},
			},
		},
	}

	sessionGet := oaOperation("Get session", "Returns authentication state. On a fresh install it creates a setup-only session and returns its CSRF token; this session can only set the initial administrator password. Logged-in sessions receive the CSRF token required by mutations.", "Session", public)
	// No 401 here: session discovery is deliberately not a protected resource,
	// so an anonymous caller gets 200 with authenticated=false.
	sessionGet["responses"] = map[string]any{
		"200": oaJSONResponse("session state", "#/components/schemas/SessionInfo"),
	}

	sessionPost := oaOperation("Log in", "Single-admin login after first-run setup. There is no default password. Identical errors are returned for any wrong password. Repeated failures trigger an exponential per-client delay.", "Session", public)
	sessionPost["requestBody"] = oaBody("#/components/schemas/LoginRequest", map[string]any{"password": ""})
	sessionPost["responses"] = map[string]any{
		"200": oaJSONResponse("logged in; sets the ytdlp_session cookie", "#/components/schemas/SessionInfo"),
		"401": oaJSONResponse("invalid password", "#/components/schemas/Error"),
		"409": oaJSONResponse("initial password setup is required", "#/components/schemas/Error"),
		"429": oaJSONResponse("rate limited; see Retry-After", "#/components/schemas/Error"),
	}

	sessionDelete := oaOperation("Log out", "Destroys the current server-side session and clears the cookie.", "Session", public)
	sessionDelete["responses"] = map[string]any{
		"200": oaJSONResponse("logged out", "#/components/schemas/LogoutResponse"),
	}

	passwordChange := oaMutation(oaOperation(
		"Set administrator password",
		"Creates the password during first-run setup or changes it for an authenticated administrator. Persists only a salted PBKDF2-SHA256 verifier, invalidates all prior sessions, and returns a fresh cookie and CSRF token. During first-run setup the request must come from the machine running the server, or carry the one-time setup token printed to the server log in an X-Setup-Token header; GET /api/v1/session reports setup_token_required when the token is needed.",
		"Session", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	passwordChange["parameters"] = append(passwordChange["parameters"].([]any), map[string]any{
		"name":        SetupTokenHeader,
		"in":          "header",
		"required":    false,
		"description": "First-run setup only, and only from a client the server cannot see as local: the one-time token printed to the server log at startup.",
		"schema":      map[string]any{"type": "string"},
	})
	passwordChange["requestBody"] = oaBody("#/components/schemas/ChangePasswordRequest", map[string]any{
		"password": "", "confirmation": "",
	})
	passwordChange["responses"] = map[string]any{
		"200": oaJSONResponse("password changed and session rotated", "#/components/schemas/SessionInfo"),
		"403": oaJSONResponse("first-run setup attempted without a valid setup token from a non-local client", "#/components/schemas/Error"),
		"422": oaJSONResponse("passwords mismatch or password policy failure", "#/components/schemas/Error"),
	}

	settingsGet := oaOperation("Get settings", "Effective manager settings and where each value came from (default, config file, environment or flag).", "Settings", protected)
	settingsGet["responses"] = map[string]any{
		"200": oaJSONResponse("settings view", "#/components/schemas/SettingsView"),
	}

	settingsPut := oaMutation(oaOperation(
		"Update settings",
		"Patch UI and download settings. Unknown fields are rejected. Concurrency must be 1..100; the runtime cap is applied before persistence and rolled back if persistence fails.",
		"Settings", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	settingsPut["requestBody"] = oaBody("#/components/schemas/SettingsUpdate", map[string]any{
		"downloads": map[string]any{"max_concurrent": 3, "extra_args": "--limit-rate 2M"},
	})
	settingsPut["responses"] = map[string]any{
		"200": oaJSONResponse("updated settings view", "#/components/schemas/SettingsView"),
		"422": oaJSONResponse("validation failed", "#/components/schemas/Error"),
		"500": oaJSONResponse("could not persist", "#/components/schemas/Error"),
	}

	ytdlpGet := oaOperation("Get yt-dlp settings", "Read-only status of the user's yt-dlp configuration file plus the current managed-block values. Editing may be disabled by deployment.", "Settings", protected)
	ytdlpGet["responses"] = map[string]any{
		"200": oaJSONResponse("status and settings", "#/components/schemas/YtDlpSettingsResponse"),
	}

	ytdlpPut := oaMutation(oaOperation(
		"Write yt-dlp managed block",
		"Splices an allowlisted settings block into the user's yt-dlp config, preserving every byte outside the markers. Refuses symlinks, non-regular files, control characters and oversized files. Only allowlisted options can be written; dangerous directives (--exec, external downloaders, plugin paths) can never be introduced. Changes apply to future downloads only.",
		"Settings", protected), "Per-session CSRF token returned by GET /api/v1/session.")
	ytdlpPut["requestBody"] = oaBody("#/components/schemas/YtDlpSettings", map[string]any{
		"downloads_dir": "/downloads", "format": "bv*+ba/b", "output_template": "%(title)s [%(id)s].%(ext)s",
	})
	ytdlpPut["responses"] = map[string]any{
		"200": oaJSONResponse("written; returns fresh status/settings", "#/components/schemas/YtDlpSettingsResponse"),
		"403": oaJSONResponse("editing disabled by deployment", "#/components/schemas/Error"),
		"409": oaJSONResponse("config could not be written safely", "#/components/schemas/Error"),
	}

	system := oaOperation("System info", "Application/runtime/tool versions, paths, uptime and readiness detail. Authenticated; paths are admin-only information.", "System", protected)
	system["responses"] = map[string]any{
		"200": oaJSONResponse("system information", "#/components/schemas/SystemInfo"),
	}

	healthz := oaOperation("Liveness", "Process liveness probe. No authentication, no private detail.", "System", public)
	healthz["responses"] = map[string]any{"200": oaTextResponse("ok")}

	readyz := oaOperation("Readiness", "Coarse readiness: manager initialized and required paths usable. Failure reasons stay coarse here; detail lives in /api/v1/system.", "System", public)
	readyz["responses"] = map[string]any{
		"200": oaJSONResponse("ready", "#/components/schemas/Readiness"),
		"503": oaJSONResponse("not ready", "#/components/schemas/Readiness"),
	}

	openapiSelf := oaOperation("OpenAPI document", "This document. Machine-readable contract for the API Explorer at #/api.", "System", protected)
	openapiSelf["responses"] = map[string]any{
		"200": oaJSONResponse("OpenAPI 3.1 document", "#/components/schemas/OpenAPIDocument"),
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "yt-dlp-manager API",
			"version":     "v1",
			"description": "Same-origin API for the yt-dlp-manager web UI. All endpoints live under one origin; browsers authenticate with the ytdlp_session cookie and mutations additionally require the per-session X-CSRF-Token header. The API is not CORS-enabled.",
			"license":     map[string]any{"name": "MIT"},
		},
		"servers": []any{
			map[string]any{"url": "/", "description": "same origin"},
		},
		"tags": []any{
			map[string]any{"name": "Session", "description": "login/logout and CSRF"},
			map[string]any{"name": "Downloads", "description": "queue and library items"},
			map[string]any{"name": "Events", "description": "live updates over Server-Sent Events"},
			map[string]any{"name": "Settings", "description": "manager and yt-dlp settings"},
			map[string]any{"name": "System", "description": "health, readiness, versions, docs"},
		},
		"paths": map[string]any{
			"/healthz": map[string]any{"get": healthz},
			"/readyz":  map[string]any{"get": readyz},
			"/api/v1/session": map[string]any{
				"get":    sessionGet,
				"post":   sessionPost,
				"delete": sessionDelete,
			},
			"/api/v1/session/password": map[string]any{"put": passwordChange},
			"/api/v1/downloads": map[string]any{
				"get":  getDownloads,
				"post": addDownloads,
			},
			"/api/v1/downloads/{id}":           map[string]any{"get": getDownload},
			"/api/v1/downloads/{id}/thumbnail": map[string]any{"get": thumbnail},
			"/api/v1/downloads/actions":        map[string]any{"post": actions},
			"/api/v1/downloads/clear":          map[string]any{"post": clear},
			"/api/v1/formats":                  map[string]any{"post": formats},
			"/api/v1/events":                   map[string]any{"get": events},
			"/api/v1/settings": map[string]any{
				"get": settingsGet,
				"put": settingsPut,
			},
			"/api/v1/settings/yt-dlp":         map[string]any{"get": ytdlpGet},
			"/api/v1/settings/yt-dlp/managed": map[string]any{"put": ytdlpPut},
			"/api/v1/system":                  map[string]any{"get": system},
			"/api/v1/openapi.json":            map[string]any{"get": openapiSelf},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"cookieAuth": map[string]any{
					"type":        "apiKey",
					"in":          "cookie",
					"name":        "ytdlp_session",
					"description": "HttpOnly, SameSite=Strict session cookie set by POST /api/v1/session.",
				},
			},
			"schemas": map[string]any{
				"Error": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"error": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"code":       map[string]any{"type": "string", "example": "invalid_state"},
								"message":    map[string]any{"type": "string"},
								"request_id": map[string]any{"type": "string"},
							},
							"required": []string{"code", "message"},
						},
					},
					"required": []string{"error"},
				},
				"Download": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":                     map[string]any{"type": "string"},
						"url":                    map[string]any{"type": "string", "format": "uri"},
						"title":                  map[string]any{"type": "string"},
						"thumbnail_url":          map[string]any{"type": "string"},
						"state":                  map[string]any{"type": "string", "enum": []string{"queued", "downloading", "paused", "completed", "failed", "deleted"}},
						"forced":                 map[string]any{"type": "boolean"},
						"pause_origin":           map[string]any{"type": "string", "enum": []string{"none", "user", "shutdown"}},
						"progress":               map[string]any{"type": "number", "minimum": 0, "maximum": 100},
						"downloaded_bytes":       map[string]any{"type": "integer", "format": "int64"},
						"total_bytes":            map[string]any{"type": "integer", "format": "int64"},
						"speed_bytes_per_second": map[string]any{"type": "integer", "format": "int64"},
						"eta_seconds":            map[string]any{"type": "integer", "format": "int64"},
						"error":                  map[string]any{"type": "string"},
						"files":                  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"added_at":               map[string]any{"type": "string", "format": "date-time"},
						"started_at":             map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"completed_at":           map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"allowed_actions":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"options":                map[string]any{"$ref": "#/components/schemas/DownloadOptions"},
						"options_summary":        map[string]any{"type": "string", "description": "the overrides rendered for display; absent when the item uses the configured defaults"},
						"options_invalid":        map[string]any{"type": "boolean", "description": "the row's saved options could not be read back after a restart; it cannot be retried, only removed and re-added, because retrying would download something different from what was requested"},
					},
					"required": []string{"id", "url", "state", "progress", "added_at", "allowed_actions"},
				},
				"DownloadEnvelope": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"download": map[string]any{"$ref": downloadRef},
					},
					"required": []string{"download"},
				},
				"DownloadList": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"downloads": map[string]any{"type": "array", "items": map[string]any{"$ref": downloadRef}},
					},
					"required": []string{"downloads"},
				},
				"AddDownloadRequest": map[string]any{
					"type":     "object",
					"required": []string{"url"},
					"properties": map[string]any{
						"url":       map[string]any{"type": "string", "maxLength": 4096},
						"start_now": map[string]any{"type": "boolean", "default": false},
						"options":   map[string]any{"$ref": "#/components/schemas/DownloadOptions"},
					},
				},
				"DownloadOptions": map[string]any{
					"type":        "object",
					"description": "Per-download overrides. Every field is optional; an omitted field means the yt-dlp configuration file decides. preset and explicit format ids are mutually exclusive, audio_format requires audio_only, and sub_langs requires subtitles \"on\".",
					"properties": map[string]any{
						"format_id":       map[string]any{"type": "string", "maxLength": 64, "pattern": "^[A-Za-z0-9_.\\-]{1,64}$", "description": "video format id from POST /api/v1/formats"},
						"audio_format_id": map[string]any{"type": "string", "maxLength": 64, "pattern": "^[A-Za-z0-9_.\\-]{1,64}$", "description": "audio format id, merged with format_id when both are given"},
						"preset":          map[string]any{"type": "string", "enum": []string{"best", "2160p", "1440p", "1080p", "720p", "480p", "360p", "audio"}, "description": "resolution ceiling, usable without probing formats first"},
						"merge_container": map[string]any{"type": "string", "enum": []string{"mp4", "mkv", "webm"}},
						"audio_only":      map[string]any{"type": "boolean", "description": "extract audio (--extract-audio)"},
						"audio_format":    map[string]any{"type": "string", "enum": []string{"aac", "alac", "flac", "m4a", "mp3", "opus", "vorbis", "wav"}},
						"subtitles":       map[string]any{"type": "string", "enum": []string{"on", "off"}, "description": "tri-state: absent means whatever the config file says"},
						"sub_langs":       map[string]any{"type": "array", "maxItems": ipc.MaxSubLangs, "items": map[string]any{"type": "string", "maxLength": 32}},
						"extra_args": map[string]any{
							"type": "string", "maxLength": ipc.MaxExtraArgsLen,
							"description": "free-form yt-dlp command line text for this download, placed last on the command line so it overrides everything: the options above, the global downloads.extra_args, and the yt-dlp config. Parsed with shell quoting rules but never run through a shell, so no expansion of any kind occurs. Accepted options are a fixed allowlist of ordinary download options (rate limiting, retries, proxy, headers, format sorting, subtitles, SponsorBlock, geo options and similar), which must be spelled in full: yt-dlp accepts unambiguous abbreviations, so a list of forbidden spellings would be bypassable. Selection, filtering and playlist options are excluded deliberately — the manager expands playlists into one entry per video, so they shape nothing, and options that skip a download exit 0 without producing a file. Anything else — an unlisted option, an abbreviation, a short flag, a bare word that yt-dlp would treat as an extra URL, or an option carrying a credential — is refused with 422 invalid_options naming it.",
						},
					},
				},
				"Format": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"format_id":            map[string]any{"type": "string"},
						"ext":                  map[string]any{"type": "string"},
						"resolution":           map[string]any{"type": "string"},
						"width":                map[string]any{"type": "integer"},
						"height":               map[string]any{"type": "integer"},
						"fps":                  map[string]any{"type": "number"},
						"vcodec":               map[string]any{"type": "string"},
						"acodec":               map[string]any{"type": "string"},
						"filesize":             map[string]any{"type": "integer", "format": "int64", "description": "bytes; see filesize_approximate"},
						"filesize_approximate": map[string]any{"type": "boolean", "description": "the size is yt-dlp's estimate, not a known length"},
						"tbr":                  map[string]any{"type": "number", "description": "total bitrate, Kbit/s"},
						"format_note":          map[string]any{"type": "string"},
						"protocol":             map[string]any{"type": "string"},
						"language":             map[string]any{"type": "string"},
						"has_video":            map[string]any{"type": "boolean"},
						"has_audio":            map[string]any{"type": "boolean"},
					},
					"required": []string{"format_id", "has_video", "has_audio"},
				},
				"FormatsRequest": map[string]any{
					"type":     "object",
					"required": []string{"url"},
					"properties": map[string]any{
						"url": map[string]any{"type": "string", "maxLength": 4096},
					},
				},
				"FormatsResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url":              map[string]any{"type": "string", "format": "uri"},
						"title":            map[string]any{"type": "string"},
						"duration_seconds": map[string]any{"type": "number"},
						"extractor":        map[string]any{"type": "string"},
						"formats":          map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Format"}},
						"truncated":        map[string]any{"type": "boolean", "description": "more formats existed than the server reports"},
					},
					"required": []string{"formats"},
				},
				"AddDownloadResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"download": map[string]any{"$ref": downloadRef},
						"start_now": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"requested": map[string]any{"type": "boolean"},
								"applied":   map[string]any{"type": "boolean"},
								"error":     map[string]any{"type": "string"},
							},
						},
					},
				},
				"ActionsRequest": map[string]any{
					"type":     "object",
					"required": []string{"action", "ids"},
					"properties": map[string]any{
						"action": map[string]any{"type": "string", "enum": []string{"pause", "resume", "start_now", "remove", "retry"}},
						"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 500, "minItems": 1, "uniqueItems": true},
					},
				},
				"ActionsResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"results": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id":    map[string]any{"type": "string"},
									"ok":    map[string]any{"type": "boolean"},
									"error": map[string]any{"$ref": "#/components/schemas/Error"},
								},
								"required": []string{"id", "ok"},
							},
						},
					},
					"required": []string{"results"},
				},
				"ClearRequest": map[string]any{
					"type":     "object",
					"required": []string{"scope"},
					"properties": map[string]any{
						"scope": map[string]any{"type": "string", "enum": clearScopeList},
					},
				},
				"ClearResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"scope":   map[string]any{"type": "string", "enum": clearScopeList},
						"removed": map[string]any{"type": "integer"},
					},
					"required": []string{"scope", "removed"},
				},
				"SessionInfo": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"authenticated":        map[string]any{"type": "boolean"},
						"csrf_token":           map[string]any{"type": "string", "description": "present for authenticated and first-run setup sessions; send as X-CSRF-Token on permitted mutations"},
						"setup_required":       map[string]any{"type": "boolean", "description": "when true, the setup-only session can only create the initial administrator password"},
						"setup_token_required": map[string]any{"type": "boolean", "description": "during first-run setup, true when this client is not local to the server and must send the setup token from the server log as X-Setup-Token"},
					},
					"required": []string{"authenticated", "setup_required"},
				},
				"ChangePasswordRequest": map[string]any{
					"type":     "object",
					"required": []string{"password", "confirmation"},
					"properties": map[string]any{
						"password":     map[string]any{"type": "string", "format": "password", "minLength": 8, "maxLength": 1024},
						"confirmation": map[string]any{"type": "string", "format": "password", "minLength": 8, "maxLength": 1024},
					},
				},
				"LogoutResponse": map[string]any{
					"type":       "object",
					"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
				},
				"SettingsView": map[string]any{
					"type":        "object",
					"description": "Effective settings; see GET for authoritative shape. ui.theme/compact, downloads.max_concurrent plus its source and downloads.extra_args, security flags, advanced paths.",
					"properties": map[string]any{
						"ui":        map[string]any{"type": "object"},
						"downloads": map[string]any{"type": "object"},
						"security":  map[string]any{"type": "object"},
						"advanced":  map[string]any{"type": "object"},
						"sources":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
					},
				},
				"SettingsUpdate": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ui": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"theme":   map[string]any{"type": "string", "enum": []string{"dark", "light", "system"}},
								"compact": map[string]any{"type": "boolean"},
							},
						},
						"downloads": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"max_concurrent": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
								"extra_args": map[string]any{
									"type": "string", "maxLength": ipc.MaxExtraArgsLen,
									"description": "free-form yt-dlp command line text applied to every download. It is placed after the yt-dlp config but BEFORE a download's own picker options and extra_args, so it acts as a default that a single download can override rather than one that overrides them. Same parsing and same allowlist as a download's own extra_args; an unusable value is rejected with 422 invalid_options naming the option, and the live manager keeps its previous value.",
								},
							},
						},
					},
				},
				"YtDlpSettings": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"downloads_dir":   map[string]any{"type": "string", "description": "absolute path under /downloads"},
						"format":          map[string]any{"type": "string", "maxLength": 500},
						"output_template": map[string]any{"type": "string", "maxLength": 500, "description": "file name pattern relative to the download directory; absolute paths, \"..\" segments, \"~\" and \"$VAR\" are rejected because yt-dlp would resolve them outside it"},
						"extract_audio":   map[string]any{"type": "boolean"},
						"audio_format":    map[string]any{"type": "string", "enum": []string{"aac", "alac", "flac", "m4a", "mp3", "opus", "vorbis", "wav"}},
						"sub_languages":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16},
						"write_subs":      map[string]any{"type": "boolean"},
						"rate_limit":      map[string]any{"type": "string", "example": "500K", "description": "e.g. 500K, 4M or a byte count"},
						"has_block":       map[string]any{"type": "boolean"},
					},
				},
				"YtDlpSettingsResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"path":       map[string]any{"type": "string"},
								"exists":     map[string]any{"type": "boolean"},
								"symlink":    map[string]any{"type": "boolean"},
								"size_bytes": map[string]any{"type": "integer"},
								"has_block":  map[string]any{"type": "boolean"},
								"problem":    map[string]any{"type": "string"},
							},
						},
						"settings":     map[string]any{"$ref": "#/components/schemas/YtDlpSettings"},
						"edit_enabled": map[string]any{"type": "boolean"},
						"warning":      map[string]any{"type": "string"},
					},
				},
				"SystemInfo": map[string]any{
					"type":        "object",
					"description": "version/runtime/tools/paths/uptime; see the live response for the authoritative shape.",
				},
				"Readiness": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status": map[string]any{"type": "string", "enum": []string{"ready", "not_ready"}},
						"reason": map[string]any{"type": "string"},
					},
				},
				"OpenAPIDocument": map[string]any{
					"type":        "object",
					"description": "This OpenAPI 3.1 document.",
				},
			},
		},
	}
}
