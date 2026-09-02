package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"yt-dlp-manager/internal/config"
	"yt-dlp-manager/internal/ipc"
)

type settingsView struct {
	UI struct {
		Theme   string `json:"theme"`
		Compact bool   `json:"compact"`
	} `json:"ui"`
	Downloads struct {
		MaxConcurrent int    `json:"max_concurrent"`
		Source        string `json:"source"`
		// ExtraArgs is yt-dlp command line text appended to every download.
		ExtraArgs string `json:"extra_args"`
	} `json:"downloads"`
	Security struct {
		AuthenticationEnabled bool   `json:"authentication_enabled"`
		UnauthenticatedMode   bool   `json:"unauthenticated_mode"`
		SecureCookie          bool   `json:"secure_cookie"`
		Listen                string `json:"listen"`
	} `json:"security"`
	Advanced struct {
		AllowYtDlpConfigEdit bool   `json:"allow_yt_dlp_config_edit"`
		StatePath            string `json:"state_path"`
		ConfigPath           string `json:"config_path"`
		YtDlpConfigPath      string `json:"yt_dlp_config_path"`
	} `json:"advanced"`
	Sources map[string]string `json:"sources"`
}

// buildSettingsViewLocked assembles the view from the mutable config. The
// caller must hold s.settingsMu. It never locks itself, so PUT handlers can
// call it while already holding the lock (no reentrant deadlock).
func (s *Server) buildSettingsViewLocked() settingsView {
	var v settingsView
	f := s.deps.Config
	v.UI.Theme = f.UI.Theme
	v.UI.Compact = f.UI.Compact
	v.Downloads.MaxConcurrent = f.Downloads.MaxConcurrent
	v.Downloads.ExtraArgs = f.Downloads.ExtraArgs
	if s.deps.Sources != nil {
		if src, ok := s.deps.Sources["downloads.max_concurrent"]; ok {
			v.Downloads.Source = string(src)
		}
	}
	v.Security.AuthenticationEnabled = !s.deps.Unauthenticated
	v.Security.UnauthenticatedMode = s.deps.Unauthenticated
	v.Security.SecureCookie = s.deps.SecureCookie
	v.Security.Listen = s.deps.Listen
	v.Advanced.AllowYtDlpConfigEdit = s.deps.AllowYtDlpConfigEdit
	v.Advanced.StatePath = s.deps.StatePath
	if s.deps.Store != nil {
		v.Advanced.ConfigPath = s.deps.Store.Path()
	}
	v.Advanced.YtDlpConfigPath = s.deps.YtDlpConfigPath
	if s.deps.Sources != nil {
		v.Sources = make(map[string]string, len(s.deps.Sources))
		for k, src := range s.deps.Sources {
			v.Sources[k] = string(src)
		}
	}
	return v
}

// buildSettingsView snapshots the settings under the per-Server config lock.
func (s *Server) buildSettingsView() settingsView {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.buildSettingsViewLocked()
}

type settingsUpdateRequest struct {
	UI struct {
		Theme   *string `json:"theme,omitempty"`
		Compact *bool   `json:"compact,omitempty"`
	} `json:"ui"`
	Downloads struct {
		MaxConcurrent *int    `json:"max_concurrent,omitempty"`
		ExtraArgs     *string `json:"extra_args,omitempty"`
	} `json:"downloads"`
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildSettingsView())
}

// handleSettingsPut serializes the read-modify-write of the shared config
// against other PUTs and against GET readers on this Server instance. The
// lock lives on the Server, so independent Server instances (tests, embedded
// adapters) never serialize each other.
func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var req settingsUpdateRequest
	if err := decodeJSON(w, r, &req, MaxBodyBytes); err != nil {
		return
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	updated := *s.deps.Config
	changed := false
	if req.UI.Theme != nil {
		updated.UI.Theme = *req.UI.Theme
		changed = true
	}
	if req.UI.Compact != nil {
		updated.UI.Compact = *req.UI.Compact
		changed = true
	}
	if req.Downloads.MaxConcurrent != nil {
		n := *req.Downloads.MaxConcurrent
		if n < 1 || n > 100 {
			writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
				"max_concurrent must be between 1 and 100")
			return
		}
		updated.Downloads.MaxConcurrent = n
		changed = true
	}
	if req.Downloads.ExtraArgs != nil {
		// Parsed and checked here so the caller is told exactly which option
		// was refused, rather than every later download failing.
		if _, err := ipc.ExtraArgs(*req.Downloads.ExtraArgs); err != nil {
			writeError(w, r, http.StatusUnprocessableEntity, codeInvalidOptions, err.Error())
			return
		}
		updated.Downloads.ExtraArgs = strings.TrimSpace(*req.Downloads.ExtraArgs)
		changed = true
	}
	if !changed {
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState, "no editable fields supplied")
		return
	}
	if err := config.Validate(&updated); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
			"settings failed validation: "+err.Error())
		return
	}

	// Apply to the live manager first, then persist, then commit memory. If
	// persistence fails the runtime change is rolled back so disk, memory,
	// and the manager never diverge.
	prevMax := s.deps.Config.Downloads.MaxConcurrent
	prevExtra := s.deps.Config.Downloads.ExtraArgs

	// Persist BEFORE touching the running manager. Raising the concurrency cap
	// immediately releases queued downloads, and a download that has started
	// cannot be un-started: if persistence then failed, the rollback would be
	// a lie — jobs would already be running under a configuration the caller
	// was told had been rejected. Writing first means the only work that ever
	// starts is work whose settings survived.
	if s.deps.Store != nil {
		if err := s.deps.Store.Save(&updated); err != nil && !errors.Is(err, config.ErrNoConfig) {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "settings could not be persisted")
			return
		}
	}

	// Arguments before concurrency: SetMaxConcurrent runs the scheduler, so
	// anything it releases must already see the new arguments.
	if updated.Downloads.ExtraArgs != prevExtra {
		if err := s.deps.Manager.SetExtraArgs(updated.Downloads.ExtraArgs); err != nil {
			// Validated above, so this is unreachable in practice; put the file
			// back rather than leave disk describing a state nothing applied.
			s.restorePersistedSettings(prevMax, prevExtra)
			writeError(w, r, http.StatusUnprocessableEntity, codeInvalidOptions, err.Error())
			return
		}
	}
	if updated.Downloads.MaxConcurrent != prevMax {
		if err := s.deps.Manager.SetMaxConcurrent(updated.Downloads.MaxConcurrent); err != nil {
			if updated.Downloads.ExtraArgs != prevExtra {
				_ = s.deps.Manager.SetExtraArgs(prevExtra)
			}
			s.restorePersistedSettings(prevMax, prevExtra)
			writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState,
				"max_concurrent could not be applied")
			return
		}
	}
	*s.deps.Config = updated
	// Lock is already held: use the locked view builder (no re-entry).
	writeJSON(w, http.StatusOK, s.buildSettingsViewLocked())
}

// restorePersistedSettings rewrites the two download fields to their previous
// values after a runtime change could not be applied. Caller must hold
// s.settingsMu.
func (s *Server) restorePersistedSettings(prevMax int, prevExtra string) {
	if s.deps.Store == nil {
		return
	}
	reverted := *s.deps.Config
	reverted.Downloads.MaxConcurrent = prevMax
	reverted.Downloads.ExtraArgs = prevExtra
	_ = s.deps.Store.Save(&reverted)
}

// --- yt-dlp managed settings ---

type ytDlpSettingsResponse struct {
	Status      config.YtDlpStatus    `json:"status"`
	Settings    *config.YtDlpSettings `json:"settings"`
	EditEnabled bool                  `json:"edit_enabled"`
	Warning     string                `json:"warning,omitempty"`
}

func (s *Server) handleYtDlpGet(w http.ResponseWriter, r *http.Request) {
	status, settings, _ := config.ReadYtDlpConfig(s.deps.YtDlpConfigPath)
	resp := ytDlpSettingsResponse{
		Status:      *status,
		Settings:    settings,
		EditEnabled: s.deps.AllowYtDlpConfigEdit,
	}
	if status.Problem != "" {
		resp.Warning = status.Problem
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleYtDlpManagedPut(w http.ResponseWriter, r *http.Request) {
	if !s.deps.AllowYtDlpConfigEdit {
		writeError(w, r, http.StatusForbidden, codeForbidden,
			"yt-dlp configuration editing is disabled; enable allow_yt_dlp_config_edit or edit the file yourself")
		return
	}
	var req config.YtDlpSettings
	if err := decodeJSON(w, r, &req, MaxBodyBytes); err != nil {
		return
	}
	req.HasBlock = true
	if err := config.ValidateYtDlpSettings(&req); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, codeInvalidState, err.Error())
		return
	}
	if err := config.WriteYtDlpManagedBlock(s.deps.YtDlpConfigPath, &req); err != nil {
		writeError(w, r, http.StatusConflict, codeInternal,
			"managed block could not be written safely: "+err.Error())
		return
	}
	status, settings, _ := config.ReadYtDlpConfig(s.deps.YtDlpConfigPath)
	writeJSON(w, http.StatusOK, ytDlpSettingsResponse{
		Status: *status, Settings: settings, EditEnabled: true,
	})
}
