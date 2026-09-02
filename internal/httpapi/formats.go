package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type formatsRequest struct {
	URL string `json:"url"`
}

type formatsResponse struct {
	URL string `json:"url"`
	*manager.FormatProbe
}

// handleFormats reports what a URL offers so the add dialog can present real
// choices instead of guesses.
//
// It is a POST even though it reads nothing: the URL belongs in a body rather
// than in a query string that lands in logs and history, and a mutation-shaped
// method keeps it behind the same CSRF check as everything else that makes the
// server spawn a process. The manager bounds the cost — a concurrency ceiling,
// a per-probe timeout and a short cache — so a UI that opens the dialog
// repeatedly cannot turn into a pile of yt-dlp processes.
func (s *Server) handleFormats(w http.ResponseWriter, r *http.Request) {
	var req formatsRequest
	if err := decodeJSON(w, r, &req, MaxBodyBytes); err != nil {
		return
	}
	url := strings.TrimSpace(req.URL)
	if !ipc.ValidURL(url) {
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidURL,
			"url must be http(s), must not exceed 4096 bytes, and may not contain control characters")
		return
	}
	probe, err := s.deps.Manager.Formats(r.Context(), url)
	if err != nil {
		switch {
		case errors.Is(err, manager.ErrFormatsUnsupported):
			writeError(w, r, http.StatusNotImplemented, codeInvalidState,
				"this server cannot list formats")
		case errors.Is(err, manager.ErrShuttingDown):
			writeError(w, r, http.StatusServiceUnavailable, codeInternal,
				"manager is shutting down")
		case r.Context().Err() != nil:
			// The client went away; there is nobody left to answer.
			return
		default:
			// 502: the request was fine, the extractor was not. The message is
			// yt-dlp's own failure text, already sanitized and bounded by the
			// manager, because "unsupported URL" or "video is private" is
			// exactly what the person pasting the link needs to read.
			writeError(w, r, http.StatusBadGateway, codeInvalidURL, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, formatsResponse{URL: url, FormatProbe: probe})
}
