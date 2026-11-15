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
	"go-harness/harness/agent"
	"go-harness/internal/durablefs"
	"go-harness/internal/localstate"
	"go-harness/internal/modelsettings"
	"go-harness/providers/model/mainmodel"
	"go-harness/thirdparty/llm"
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
	endpoint, err := m.snapshot.Catalog.Resolve(choice)
	if err != nil {
		return agent.TurnModel{}, err
	}
	route := agent.ModelRoute{Provider: choice.EndpointID, Model: choice.ModelID}
	model := m.base
	model.Config = mainmodel.Config{
		Default: route,
		Endpoints: map[string]mainmodel.Endpoint{route.Provider: {
			BaseURL: endpoint.BaseURL, APIKey: endpoint.APIKey, Protocol: llm.Responses,
		}},
	}
	return agent.TurnModel{Model: model, Route: route}, nil
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
		if snapshot.Revision < m.snapshot.Revision {
			m.mu.Unlock()
			http.Error(w, "stale model settings", http.StatusConflict)
			return
		}
		m.snapshot = snapshot

		m.mu.Unlock()
		wake()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *hostModels) resolver(sessionID string) func(context.Context) (agent.TurnModel, error) {
	return func(context.Context) (agent.TurnModel, error) { return m.route(sessionID) }
}
