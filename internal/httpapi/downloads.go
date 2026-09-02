package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"yt-dlp-manager/internal/ipc"
	"yt-dlp-manager/internal/manager"
)

type addDownloadRequest struct {
	URL      string `json:"url"`
	StartNow bool   `json:"start_now"`
	// Options are per-download overrides that beat the yt-dlp configuration
	// file for this item only. Absent means "use the configured defaults",
	// which is what the plain paste-and-enter path sends.
	Options ipc.Options `json:"options"`
}

type startNowResult struct {
	Requested bool   `json:"requested"`
	Applied   bool   `json:"applied"`
	Error     string `json:"error,omitempty"`
}

type addDownloadResponse struct {
	Download Download       `json:"download"`
	StartNow startNowResult `json:"start_now"`
}

func (s *Server) handleDownloadAdd(w http.ResponseWriter, r *http.Request) {
	var req addDownloadRequest
	if err := decodeJSON(w, r, &req, MaxBodyBytes); err != nil {
		return
	}
	url := strings.TrimSpace(req.URL)
	if !ipc.ValidURL(url) {
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidURL,
			"url must be http(s), must not exceed 4096 bytes, and may not contain control characters")
		return
	}
	opts := req.Options.Normalize()
	if verr := opts.Validate(); verr != nil {
		// 422 rather than 400: the body parsed, the choice inside it is what
		// cannot be turned into yt-dlp arguments, and the message says which.
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidOptions, verr.Error())
		return
	}
	id, err := s.deps.Manager.AddWithOptions(url, opts)
	if err != nil {
		// A URL already represented by a queued/running/completed entry is a
		// conflict, not a bad request: the caller pasted something valid that
		// this manager is already tracking, and the message says which.
		var dup *manager.DuplicateError
		switch {
		case errors.As(err, &dup):
			writeError(w, r, http.StatusConflict, codeDuplicateURL, dup.Error())
		case errors.Is(err, manager.ErrItemTooLarge):
			// 422: the URL is well formed, it just cannot be stored.
			writeError(w, r, http.StatusUnprocessableEntity, codeInvalidURL, err.Error())
		case errors.Is(err, manager.ErrQueueFull):
			// 507: the request is valid, the server just has nowhere to put it.
			writeError(w, r, http.StatusInsufficientStorage, codeInvalidState, err.Error())
		case errors.Is(err, manager.ErrShuttingDown):
			writeError(w, r, http.StatusServiceUnavailable, codeInternal, "manager is shutting down")
		default:
			writeError(w, r, http.StatusBadRequest, codeInvalidURL, "download could not be queued")
		}
		return
	}
	res := addDownloadResponse{StartNow: startNowResult{Requested: req.StartNow}}
	if req.StartNow {
		if serr := s.deps.Manager.StartNow(id); serr != nil {
			// The item stays normally queued; report the partial result honestly.
			res.StartNow.Error = "could not force start; download remains queued"
		} else {
			res.StartNow.Applied = true
		}
	}
	it, ok := s.deps.Manager.Get(id)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "queued item vanished")
		return
	}
	res.Download = toDownload(it)
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleDownloadsList(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	stateFilter := strings.TrimSpace(r.URL.Query().Get("state"))

	items := s.deps.Manager.List()
	filtered := make([]ipc.Item, 0, len(items))
	for _, it := range items {
		if stateFilter != "" && string(it.State) != stateFilter {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(it.Title), q) &&
			!strings.Contains(strings.ToLower(it.URL), q) {
			continue
		}
		filtered = append(filtered, it)
	}
	sortQueue(filtered)
	writeJSON(w, http.StatusOK, map[string]any{
		"downloads": toDownloads(filtered),
	})
}

func (s *Server) handleDownloadGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validDownloadID(id) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such download")
		return
	}
	it, ok := s.deps.Manager.Get(id)
	if !ok {
		writeError(w, r, http.StatusNotFound, codeNotFound, "no such download")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"download": toDownload(it)})
}

type batchActionRequest struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

type batchResult struct {
	ID    string    `json:"id"`
	OK    bool      `json:"ok"`
	Error *apiError `json:"error,omitempty"`
}

type batchResponse struct {
	Results []batchResult `json:"results"`
}

// Deleting media files is deliberately not an action. The app downloads into a
// directory the operator owns (a host bind mount, in the container); removing
// files from there is the file manager's job, and keeping that capability out
// of a network service removes the entire class of "a path bug destroys data"
// risk. Removing a row still cleans up yt-dlp's own scratch (.part/.ytdl).
var validActions = map[string]bool{
	"pause": true, "resume": true, "start_now": true,
	"remove": true, "retry": true,
}

// maxBatchIDs bounds a single batch request.
const maxBatchIDs = 500

func (s *Server) handleBatchActions(w http.ResponseWriter, r *http.Request) {
	var req batchActionRequest
	if err := decodeJSON(w, r, &req, MaxBodyBytes); err != nil {
		return
	}
	switch {
	case !validActions[req.Action]:
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
			"unknown action (allowed: pause, resume, start_now, remove, retry)")
		return
	case len(req.IDs) == 0:
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState, "ids must not be empty")
		return
	case len(req.IDs) > maxBatchIDs:
		writeError(w, r, http.StatusUnprocessableEntity, codeTooLarge,
			"at most 500 ids per batch")
		return
	}

	seen := make(map[string]bool, len(req.IDs))
	results := make([]batchResult, 0, len(req.IDs))
	applyOne := func(id string) error {
		mgr := s.deps.Manager
		switch req.Action {
		case "pause":
			return mgr.Pause(id)
		case "resume", "retry":
			return mgr.Resume(id)
		case "start_now":
			return mgr.StartNow(id)
		case "remove":
			return mgr.Cancel(id)
		}
		return errors.New("unhandled action")
	}

	for _, id := range req.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			results = append(results, batchResult{ID: id, OK: false, Error: &apiError{
				Code: codeInvalidState, Message: "duplicate or empty id",
			}})
			continue
		}
		seen[id] = true
		if err := applyOne(id); err != nil {
			results = append(results, batchResult{ID: id, OK: false, Error: &apiError{
				Code:    actionErrorCode(err),
				Message: actionErrorMessage(err),
			}})
			continue
		}
		results = append(results, batchResult{ID: id, OK: true})
	}

	// Per-ID results are the contract; one stale selection must not hide
	// successful actions, so the overall status stays 200 even when some
	// individual actions failed.
	writeJSON(w, http.StatusOK, batchResponse{Results: results})
}

func actionErrorCode(err error) string {
	switch {
	case errors.Is(err, manager.ErrNotFound):
		return codeNotFound
	case errors.Is(err, manager.ErrDuplicate):
		return codeDuplicateURL
	case errors.Is(err, manager.ErrInvalidState):
		return codeInvalidState
	default:
		return codeInternal
	}
}

func actionErrorMessage(err error) string {
	// A retry refused because another entry now holds the same URL has to say
	// so; the generic invalid-state wording would send the user looking for a
	// state problem that is not there.
	var dup *manager.DuplicateError
	switch {
	case errors.Is(err, manager.ErrNotFound):
		return "no such download"
	case errors.As(err, &dup):
		return dup.Error()
	case errors.Is(err, manager.ErrInvalidState):
		return "download is not in a state that allows this action"
	default:
		return "action failed"
	}
}
