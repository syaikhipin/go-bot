package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"go-bot/harness/agent"
	"go-bot/internal/durablefs"
	"go-bot/internal/localstate"
	"go-bot/internal/modelsettings"
	"go-bot/providers/model/mainmodel"
	"go-bot/thirdparty/llm"
)

type hostModels struct {
	mu           sync.RWMutex
	snapshot     modelsettings.Snapshot
	base         mainmodel.Model
	scheduledDir string
	scheduled    map[string]modelsettings.Selection
}

func (m *hostModels) defaultModel() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot.Catalog.Default().ModelID
}

func (m *hostModels) route(sessionID string) (agent.TurnModel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	choice, ok := m.snapshot.Sessions[sessionID]
	if !ok {
		choice = m.scheduled[sessionID]
	}
	route := agent.ModelRoute{Provider: choice.EndpointID, Model: choice.ModelID}
	model, err := m.selectedLocked(route)
	if err != nil {
		return agent.TurnModel{}, err
	}
	return agent.TurnModel{Model: model, Route: route}, nil
}

// Background research follows the main conversation's explicit selection.
// Only a genuinely absent selection can use the valid account default; a
// removed endpoint or model must not silently switch to something else.
func (m *hostModels) backgroundRoute(mainID string) (agent.TurnModel, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	choice, exists := m.snapshot.Sessions[mainID]
	if !exists {
		choice, exists = m.scheduled[mainID]
	}
	if !exists {
		choice = m.snapshot.Catalog.Default()
	}
	route := agent.ModelRoute{Provider: choice.EndpointID, Model: choice.ModelID}
	model, err := m.selectedLocked(route)
	return agent.TurnModel{Model: model, Route: route}, err
}

func (m *hostModels) selected(_ context.Context, route agent.ModelRoute) (agent.Model, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selectedLocked(route)
}

func (m *hostModels) selectedLocked(route agent.ModelRoute) (agent.Model, error) {
	endpoint, err := m.snapshot.Catalog.Resolve(modelsettings.Selection{EndpointID: route.Provider, ModelID: route.Model})
	if err != nil {
		return nil, err
	}
	model := m.base
	model.Config = mainmodel.Config{
		Default: route,
		Endpoints: map[string]mainmodel.Endpoint{route.Provider: {
			BaseURL: endpoint.BaseURL, APIKey: endpoint.APIKey, Protocol: llm.Protocol(modelsettings.NormalizeProtocol(endpoint.Protocol)),
		}},
	}
	return model, nil
}

func (m *hostModels) defaultProtocol() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return modelsettings.NormalizeProtocol(m.snapshot.Catalog.Endpoints[m.snapshot.Catalog.DefaultEndpointID].Protocol)
}

// Host-created Schedule Sessions keep their credential-free selection on disk.
func (m *hostModels) loadReservations(dir string) error {
	if err := durablefs.EnsureDirectory(dir); err != nil {
		return err
	}
	m.scheduledDir = dir
	m.scheduled = make(map[string]modelsettings.Selection)
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		id, found := strings.CutSuffix(file.Name(), ".json")
		if !found || uuid.Validate(id) != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return err
		}
		var selection modelsettings.Selection
		if err := json.Unmarshal(data, &selection); err != nil {
			return errors.New("invalid scheduled session model reservation")
		}
		m.scheduled[id] = selection
	}
	return nil
}

func (m *hostModels) reserve(sessionID string) error {
	if uuid.Validate(sessionID) != nil {
		return errors.New("invalid scheduled session ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	choice, exists := m.snapshot.Sessions[sessionID]
	if !exists {
		choice, exists = m.scheduled[sessionID]
	}
	if exists {
		_, err := m.snapshot.Catalog.Resolve(choice)
		return err
	}
	choice = m.snapshot.Catalog.Default()
	if _, err := m.snapshot.Catalog.Resolve(choice); err != nil {
		return err
	}
	return m.saveScheduledSelection(sessionID, choice)
}

func (m *hostModels) saveScheduledSelection(sessionID string, choice modelsettings.Selection) error {
	data, err := json.Marshal(choice)
	if err != nil {
		return err
	}
	if err := localstate.WritePrivate(filepath.Join(m.scheduledDir, sessionID+".json"), data); err != nil {
		return err
	}
	m.scheduled[sessionID] = choice
	return nil
}

func (m *hostModels) handleScheduledSelection(w http.ResponseWriter, r *http.Request, id string, wake func()) {
	if uuid.Validate(id) != nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		choice, exists := m.scheduled[id]
		m.mu.RUnlock()
		if !exists {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(choice)
	case http.MethodPut:
		var choice modelsettings.Selection
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&choice); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
			http.Error(w, "invalid model selection", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		if _, exists := m.scheduled[id]; !exists {
			m.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		if _, err := m.snapshot.Catalog.Resolve(choice); err != nil {
			m.mu.Unlock()
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		err := m.saveScheduledSelection(id, choice)
		m.mu.Unlock()
		if err != nil {
			http.Error(w, "could not save model selection", http.StatusServiceUnavailable)
			return
		}
		wake()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *hostModels) handle(w http.ResponseWriter, r *http.Request, wake func()) {
	switch r.Method {
	case http.MethodGet:
		m.mu.RLock()
		revision := m.snapshot.Revision
		m.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Revision int64 `json:"revision"`
		}{revision})
	case http.MethodPut:
		var snapshot modelsettings.Snapshot
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&snapshot); err != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || snapshot.Revision < 0 {
			http.Error(w, "invalid model settings", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		for _, endpoint := range snapshot.Catalog.Endpoints {
			if !modelsettings.ValidProtocol(endpoint.Protocol) {
				m.mu.Unlock()
				http.Error(w, "invalid model protocol", http.StatusBadRequest)
				return
			}
		}
		if snapshot.Revision < m.snapshot.Revision {
			m.mu.Unlock()
			http.Error(w, "stale model settings", http.StatusConflict)
			return
		}
		changed := m.snapshot.Revision != snapshot.Revision
		m.snapshot = snapshot

		m.mu.Unlock()
		if changed {
			wake()
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *hostModels) resolver(sessionID string) func(context.Context) (agent.TurnModel, error) {
	return func(context.Context) (agent.TurnModel, error) { return m.route(sessionID) }
}
