package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"go-harness/internal/config"
	"go-harness/internal/modelsettings"
)

const settingsPath = "/v1/settings/models"

type publicEndpoint struct {
	Name      string   `json:"name"`
	BaseURL   string   `json:"baseUrl"`
	HasAPIKey bool     `json:"hasApiKey"`
	Models    []string `json:"models"`
}

type modelStatus struct {
	Selection *modelsettings.Selection `json:"selection"`
	Status    string                   `json:"status"`
}

func modelView(c modelsettings.Catalog, selection modelsettings.Selection) modelStatus {
	status := modelStatus{Status: "ready"}
	if selection != (modelsettings.Selection{}) {
		status.Selection = &selection
	}
	switch _, err := c.Resolve(selection); err {
	case modelsettings.ErrUnconfigured:
		status.Status = "unconfigured"
	case modelsettings.ErrUnavailable:
		status.Status = "unavailable"
	}
	return status
}

func (m *Manager) requestHost(ctx context.Context, record Account, method, path string, input, output any) (int, error) {
	target, err := url.Parse(record.HostEndpoint)
	if err != nil || target.Scheme != "http" || target.Host == "" {
		return 0, errHostUnavailable
	}
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, record.HostEndpoint+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+record.HostToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if output != nil && res.StatusCode >= 200 && res.StatusCode < 300 {
		err = decodeJSON(io.LimitReader(res.Body, 1<<20), output)
	}
	return res.StatusCode, err
}

const settingsSavedHostApplyFailed = "settings_saved_host_apply_failed"

func (m *Manager) syncAccountSettings(ctx context.Context, id string, record Account) error {
	state, err := m.Store.LoadModels(ctx, id)
	if err != nil {
		return err
	}
	if err := m.syncModels(ctx, record, state); err != nil {
		return err
	}
	return errors.Join(m.syncProviderSettings(ctx, record, searchSettings), m.syncProviderSettings(ctx, record, imageSettings))
}

func (m *Manager) hostHasModelRevision(ctx context.Context, record Account, revision int64) bool {
	var current struct {
		Revision int64 `json:"revision"`
	}
	code, err := m.requestHost(ctx, record, http.MethodGet, modelsettings.InternalPath, nil, &current)
	return err == nil && code == http.StatusOK && current.Revision == revision
}

func (m *Manager) syncModels(ctx context.Context, record Account, state modelsettings.Snapshot) error {
	code, err := m.requestHost(ctx, record, http.MethodPut, modelsettings.InternalPath, state, nil)
	if err != nil || code != http.StatusNoContent {
		return errors.New("host_model_settings_unavailable")
	}
	return nil
}

func (m *Manager) sessionModelSelection(ctx context.Context, record Account, state modelsettings.Snapshot, sessionID string) (modelsettings.Selection, bool, error) {
	if choice, exists := state.Sessions[sessionID]; exists {
		return choice, false, nil
	}
	var choice modelsettings.Selection
	code, err := m.requestHost(ctx, record, http.MethodGet, modelsettings.ScheduledSelectionPrefix+sessionID, nil, &choice)
	if err == nil && code == http.StatusNotFound {
		return choice, false, nil
	}
	if err != nil || code != http.StatusOK {
		return choice, false, errors.New("session_model_unavailable")
	}
	return choice, true, nil
}

type endpointPatch struct {
	Name    *string   `json:"name"`
	BaseURL *string   `json:"baseUrl"`
	APIKey  *string   `json:"apiKey"`
	Models  *[]string `json:"models"`
}

func (p endpointPatch) apply(endpoint modelsettings.Endpoint) (modelsettings.Endpoint, error) {
	if p.Name != nil {
		endpoint.Name = strings.TrimSpace(*p.Name)
	}
	if p.BaseURL != nil {
		candidate := strings.TrimRight(strings.TrimSpace(*p.BaseURL), "/")
		oldURL, _ := url.Parse(endpoint.BaseURL)
		newURL, _ := url.Parse(candidate)
		if endpoint.BaseURL != "" && p.APIKey == nil && newURL != nil && oldURL != nil &&
			(!strings.EqualFold(newURL.Host, oldURL.Host) || newURL.Scheme != oldURL.Scheme) {
			return endpoint, errors.New("api_key_required_for_new_origin")
		}
		endpoint.BaseURL = candidate
	}
	if p.APIKey != nil {
		endpoint.APIKey = strings.TrimSpace(*p.APIKey)
	}
	if p.Models != nil {
		endpoint.Models = *p.Models
	}
	if endpoint.Name == "" || len(endpoint.Name) > 128 || len(endpoint.BaseURL) > 2048 ||
		len(endpoint.APIKey) > 4096 || strings.ContainsAny(endpoint.APIKey, "\r\n\x00") ||
		len(endpoint.Models) == 0 || len(endpoint.Models) > 100 {
		return endpoint, errors.New("invalid_model_endpoint")
	}
	seen := make(map[string]bool, len(endpoint.Models))
	for _, model := range endpoint.Models {
		if len(model) > 256 || strings.TrimSpace(model) != model || strings.ContainsAny(model, "\r\n\x00") ||
			config.ValidateModel(endpoint.APIKey, endpoint.BaseURL, model) != nil || seen[model] {
			return endpoint, errors.New("invalid_model_endpoint")
		}
		seen[model] = true
	}
	return endpoint, nil
}

func modelInput(w http.ResponseWriter, r *http.Request, input any) bool {
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 64<<10), input); err != nil {
		httpError(w, http.StatusBadRequest, "invalid_model_settings")
		return false
	}
	return true
}

func (m *Manager) modelSettingsHTTP(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	lock := m.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	record, state, ok := m.readModels(w, r, id)
	if !ok {
		return
	}
	catalog := state.Catalog
	if catalog.Endpoints == nil {
		catalog.Endpoints = make(map[string]modelsettings.Endpoint)
	}
	path := strings.TrimPrefix(r.URL.Path, settingsPath)
	switch {
	case path == "" && r.Method == http.MethodGet:
		endpoints := make(map[string]publicEndpoint, len(catalog.Endpoints))
		for key, endpoint := range catalog.Endpoints {
			endpoints[key] = publicEndpoint{endpoint.Name, endpoint.BaseURL, endpoint.APIKey != "", endpoint.Models}
		}
		writeJSON(w, http.StatusOK, struct {
			DefaultEndpointID string                    `json:"defaultEndpointId"`
			Endpoints         map[string]publicEndpoint `json:"endpoints"`
		}{catalog.DefaultEndpointID, endpoints})
		return
	case path == "/endpoints" && r.Method == http.MethodPost:
		var patch endpointPatch
		if !modelInput(w, r, &patch) {
			return
		}
		endpoint, err := patch.apply(modelsettings.Endpoint{})
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		endpointID := uuid.NewString()
		if len(catalog.Endpoints) == 0 {
			catalog.DefaultEndpointID = endpointID
		}
		catalog.Endpoints[endpointID] = endpoint
		m.finishCatalog(w, ctx, id, record, state, catalog, http.StatusCreated, endpointID)
		return
	case path == "/default" && r.Method == http.MethodPut:
		var input struct {
			EndpointID string `json:"endpointId"`
		}
		if !modelInput(w, r, &input) {
			return
		}
		if _, exists := catalog.Endpoints[input.EndpointID]; input.EndpointID != "" && !exists {
			httpError(w, http.StatusBadRequest, "model_unavailable")
			return
		}
		catalog.DefaultEndpointID = input.EndpointID
	default:
		key, found := strings.CutPrefix(path, "/endpoints/")
		endpoint, exists := catalog.Endpoints[key]
		if !found || !exists || !slices.Contains([]string{http.MethodPatch, http.MethodDelete}, r.Method) {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			delete(catalog.Endpoints, key)
			if catalog.DefaultEndpointID == key {
				catalog.DefaultEndpointID = ""
			}
		} else {
			var patch endpointPatch
			if !modelInput(w, r, &patch) {
				return
			}
			updated, err := patch.apply(endpoint)
			if err != nil {
				httpError(w, http.StatusBadRequest, err.Error())
				return
			}
			catalog.Endpoints[key] = updated
		}
	}
	m.finishCatalog(w, ctx, id, record, state, catalog, http.StatusOK, "")
}

func (m *Manager) finishCatalog(w http.ResponseWriter, ctx context.Context, id string, record Account, state modelsettings.Snapshot, catalog modelsettings.Catalog, code int, endpointID string) {
	revision, err := m.Store.SaveModelCatalog(ctx, id, catalog)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "model_settings_write_failed")
		return
	}
	state.Catalog, state.Revision = catalog, revision
	if record.DesiredState == running && record.HostEndpoint != "" && m.syncModels(ctx, record, state) != nil {
		httpError(w, http.StatusServiceUnavailable, settingsSavedHostApplyFailed)
		return
	}
	writeJSON(w, code, map[string]string{"status": "ready", "endpointId": endpointID})
}

func (m *Manager) readModels(w http.ResponseWriter, r *http.Request, id string) (Account, modelsettings.Snapshot, bool) {
	record, err := m.Store.Get(r.Context(), id)
	if errors.Is(err, ErrAccountNotFound) {
		httpError(w, http.StatusNotFound, "account not found")
		return record, modelsettings.Snapshot{}, false
	}
	if err == nil {
		var state modelsettings.Snapshot
		state, err = m.Store.LoadModels(r.Context(), id)
		if err == nil {
			return record, state, true
		}
	}
	httpError(w, http.StatusServiceUnavailable, "model_settings_unavailable")
	return record, modelsettings.Snapshot{}, false
}

func (m *Manager) sessionModelHTTP(w http.ResponseWriter, r *http.Request, id, sessionID string) {
	ctx := r.Context()
	lock := m.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	record, state, ok := m.readModels(w, r, id)
	if !ok {
		return
	}
	code, err := m.requestHost(ctx, record, http.MethodGet, "/internal/host/sessions/"+sessionID+"/exists", nil, nil)
	if code == http.StatusNotFound {
		http.NotFound(w, r)
		return
	}
	if err != nil || code != http.StatusNoContent {
		httpError(w, http.StatusServiceUnavailable, "session_unavailable")
		return
	}
	choice, scheduled, err := m.sessionModelSelection(ctx, record, state, sessionID)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "session_model_unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, modelView(state.Catalog, choice))
	case http.MethodPut:
		var input modelsettings.Selection
		if !modelInput(w, r, &input) {
			return
		}
		if _, err := state.Catalog.Resolve(input); err != nil {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
		if scheduled {
			code, err := m.requestHost(ctx, record, http.MethodPut, modelsettings.ScheduledSelectionPrefix+sessionID, input, nil)
			if err != nil || code != http.StatusNoContent {
				httpError(w, http.StatusServiceUnavailable, "session_model_write_failed")
				return
			}
		} else {
			if input != choice {
				state.Revision, err = m.Store.SaveSessionModel(ctx, id, sessionID, input)
				if err != nil {
					httpError(w, http.StatusServiceUnavailable, "model_settings_write_failed")
					return
				}
				state.Sessions[sessionID] = input
			}
			if m.syncModels(ctx, record, state) != nil {
				httpError(w, http.StatusServiceUnavailable, settingsSavedHostApplyFailed)
				return
			}
		}
		writeJSON(w, http.StatusOK, modelView(state.Catalog, input))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *Manager) createSessionHTTP(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		WorkspaceSessionID string `json:"workspaceSessionId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := decodeJSON(r.Body, &input); (err != nil && !errors.Is(err, io.EOF)) ||
		(input.WorkspaceSessionID != "" && uuid.Validate(input.WorkspaceSessionID) != nil) {
		httpError(w, http.StatusBadRequest, "invalid_workspace_session")
		return
	}
	ctx := r.Context()
	lock := m.accountLock(id)
	lock.Lock()
	defer lock.Unlock()
	record, state, ok := m.readModels(w, r, id)
	if !ok {
		return
	}
	choice := state.Catalog.Default()
	if _, err := state.Catalog.Resolve(choice); err != nil {
		httpError(w, http.StatusConflict, "model_default_required")
		return
	}
	if record.DesiredState != running || record.HostEndpoint == "" {
		httpError(w, http.StatusServiceUnavailable, "Host is unavailable; reopen the account")
		return
	}
	sessionID := uuid.NewString()
	revision, err := m.Store.SaveSessionModel(ctx, id, sessionID, choice)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "model_settings_write_failed")
		return
	}
	state.Sessions[sessionID], state.Revision = choice, revision
	if m.syncModels(ctx, record, state) != nil {
		httpError(w, http.StatusServiceUnavailable, "host_model_settings_unavailable")
		return
	}
	payload := map[string]string{"sessionId": sessionID}
	if input.WorkspaceSessionID != "" {
		payload["workspaceSessionId"] = input.WorkspaceSessionID
	}
	code, err := m.requestHost(ctx, record, http.MethodPost, "/internal/host/sessions", payload, nil)
	if code == http.StatusNotFound && input.WorkspaceSessionID != "" {
		httpError(w, http.StatusNotFound, "workspace_session_not_found")
		return
	}
	if err != nil || code != http.StatusCreated {
		httpError(w, http.StatusServiceUnavailable, "session_creation_unconfirmed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"sessionId": sessionID})
}
