package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type clearRequest struct {
	Scope string `json:"scope"`
}

type clearResponse struct {
	Scope   string `json:"scope"`
	Removed int    `json:"removed"`
}

// clearScopeStates maps the narrowing scopes onto the manager states they
// drop. "finished" is deliberately the union of the other two plus deleted:
// the three together are exactly what the Library calls history.
//
// "all" is not here — it also cancels active downloads, which is a different
// operation on a different manager method, not a wider state set.
var clearScopeStates = map[string][]ipc.State{
	"finished":  {ipc.StateCompleted, ipc.StateFailed, ipc.StateDeleted},
	"completed": {ipc.StateCompleted},
	"failed":    {ipc.StateFailed},
	"deleted":   {ipc.StateDeleted},
}

// clearScopeList is the enum as the error messages and the OpenAPI document
// spell it, in the order the UI offers it. Ranging over the map instead would
// reorder both on every request.
var clearScopeList = []string{"completed", "failed", "deleted", "finished", "all"}

// clearScopeHelp names the accepted scopes for a rejection message, so the
// advice cannot drift from the enum the handler actually applies.
var clearScopeHelp = func() string {
	quoted := make([]string, len(clearScopeList))
	for i, scope := range clearScopeList {
		quoted[i] = strconv.Quote(scope)
	}
	return strings.Join(quoted, ", ")
}()

// handleDownloadsClear implements POST /api/v1/downloads/clear. The scope is a
// fixed enum: "completed", "failed" and "deleted" each drop one terminal state,
// "finished" drops all three, and "all" additionally stops active downloads and
// removes every row. Downloaded media is kept by every scope.
//
// Client-supplied IDs are never involved; the manager's own bulk methods do
// the work so cancellation and file semantics stay in one place. That is also
// why a narrowed scope is a server-side state filter rather than the client
// sending the ids it happens to know about: its view can be stale, and a
// 20,000-row history would otherwise go over the wire as forty batches.
func (s *Server) handleDownloadsClear(w http.ResponseWriter, r *http.Request) {
	var req clearRequest
	if err := decodeJSON(w, r, &req, 4096); err != nil {
		return
	}
	if req.Scope == "" {
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
			"scope is required: "+clearScopeHelp)
		return
	}
	var (
		n   int
		err error
	)
	// "all" is the only scope that is not a state filter: it cancels active
	// work as well, so it goes to a different manager method.
	switch states, ok := clearScopeStates[req.Scope]; {
	case ok:
		n, err = s.deps.Manager.ClearStates(states...)
	case req.Scope == "all":
		n, err = s.deps.Manager.ClearAll()
	default:
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
			"unknown scope (allowed: "+clearScopeHelp+")")
		return
	}
	if err != nil {
		writeClearError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clearResponse{Scope: req.Scope, Removed: n})
}

func writeClearError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, manager.ErrShuttingDown) {
		writeError(w, r, http.StatusServiceUnavailable, codeInternal, err.Error())
		return
	}
	writeError(w, r, http.StatusInternalServerError, codeInternal, "downloads could not be cleared")
}
