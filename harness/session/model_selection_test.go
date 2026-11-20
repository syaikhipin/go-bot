package sessionformat

import (
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
)

func TestModelSelectionMatchesPinnedUpstreamProjectionAndAgentModel(t *testing.T) {
	data, err := os.ReadFile("testdata/model-selection-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var actions []struct {
		Kind  string
		Value json.RawMessage
	}
	if err := json.Unmarshal(data, &actions); err != nil {
		t.Fatal(err)
	}
	store := newLiveFileStore(t)
	header := v4TestHeader("model-selection")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(t.Context(), header)
	if err != nil {
		t.Fatal(err)
	}
	live, err := NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	for _, input := range []EventInput{{Type: "turn/start", Data: json.RawMessage(`{"turn":1}`)}, {Type: "step/start", Data: json.RawMessage(`{"turn":1,"step":1}`)}} {
		if _, err := live.Append(input); err != nil {
			t.Fatal(err)
		}
	}
	type snapshot struct {
		Current         *ModelSelection     `json:"current"`
		State           ModelSelectionState `json:"state"`
		Seq             uint64              `json:"seq"`
		SelectionEvents int                 `json:"selectionEvents"`
	}
	var snapshots []snapshot
	var options ModelSelection
	for _, action := range actions {
		switch action.Kind {
		case "options":
			options = ModelSelection{}
			err = json.Unmarshal(action.Value, &options)
		case "select":
			var selection ModelSelection
			if err = json.Unmarshal(action.Value, &selection); err == nil {
				err = live.SelectModel(selection)
			}
		case "header":
			value, _ := json.Marshal(map[string]json.RawMessage{"header": action.Value})
			_, err = live.Append(EventInput{Type: "request/header", Data: value})
		case "clear":
			_, err = live.Append(EventInput{Type: "model/selection", Data: json.RawMessage(`null`)})
		}
		if err != nil {
			t.Fatal(action.Kind, err)
		}
		state, err := live.ModelSelection()
		if err != nil {
			t.Fatal(err)
		}
		current := state.Current()
		if current == nil && options.Provider != "" && options.Model != "" {
			copy := options
			current = &copy
		}
		selected := 0
		for _, event := range live.Events() {
			if event.Type == "model/selection" {
				selected++
			}
		}
		snapshots = append(snapshots, snapshot{current, state, live.Position().Seq, selected})
	}
	data, err = os.ReadFile("testdata/model-selection-upstream.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected []snapshot
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshots, expected) {
		actual, _ := json.MarshalIndent(snapshots, "", "  ")
		t.Fatalf("Go model selection differs from the pinned upstream oracle:\n%s", actual)
	}
	for _, input := range []EventInput{{Type: "step/end", Data: json.RawMessage(`{"turn":1,"step":1}`)}, {Type: "turn/end", Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}} {
		if _, err := live.Append(input); err != nil {
			t.Fatal(err)
		}
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	journal, artifact, err := store.Open(t.Context(), header.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreLiveSession(artifact, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	state, err := restored.ModelSelection()
	if err != nil || !reflect.DeepEqual(state, expected[len(expected)-1].State) {
		t.Fatal("actual disk restore changed the model projection", state, err)
	}
	state.LastUsed.Model = "caller mutation"
	again, err := restored.ModelSelection()
	if err != nil || again.LastUsed.Model == "caller mutation" {
		t.Fatal("model projection leaked mutable state", err)
	}
}

func TestConcurrentModelSelectRecordsOnceWithoutFlushing(t *testing.T) {
	live, err := NewLiveSession(v4TestHeader("model-select"), &memoryV4Journal{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	selection := ModelSelection{Provider: "p", Model: "m"}
	var group sync.WaitGroup
	for index := 0; index < 16; index++ {
		group.Go(func() {
			if err := live.SelectModel(selection); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if live.Position().Seq != 1 || live.DurableCursor() != -1 {
		t.Fatal("equal selections were duplicated or select introduced an implicit checkpoint")
	}
}
