package httpapi

import (
	"errors"
	"net/http"

	"yt-dlp-manager/internal/manager"
)

type clearRequest struct {
	Scope string `json:"scope"`
}

type clearResponse struct {
	Scope   string `json:"scope"`
	Removed int    `json:"removed"`
}

// handleDownloadsClear implements POST /api/v1/downloads/clear. The scope is
// a fixed enum: "finished" drops completed/failed/deleted history (files are
// kept) and "all" additionally stops active downloads and removes every row.
// Client-supplied IDs are never involved; the manager's own bulk methods do
// the work so cancellation and file semantics stay in one place.
func (s *Server) handleDownloadsClear(w http.ResponseWriter, r *http.Request) {
	var req clearRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	switch req.Scope {
	case "finished":
		n, err := s.deps.Manager.ClearFinished()
		if err != nil {
			writeClearError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, clearResponse{Scope: req.Scope, Removed: n})
	case "all":
		n, err := s.deps.Manager.ClearAll()
		if err != nil {
			writeClearError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, clearResponse{Scope: req.Scope, Removed: n})
	case "":
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
			`scope is required: "finished" or "all"`)
	default:
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
			`unknown scope (allowed: "finished", "all")`)
	}
}

func writeClearError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, manager.ErrShuttingDown) {
		writeError(w, r, http.StatusServiceUnavailable, codeInternal, err.Error())
		return
	}
	writeError(w, r, http.StatusInternalServerError, codeInternal, "downloads could not be cleared")
}
