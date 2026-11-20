package agent

import (
	"context"
	"testing"

	"go-bot/harness/session"
	"go-bot/internal/modeldata"
)

type modelOptionsProbe struct {
	routes []ModelRoute
	caps   []int
}

func (m *modelOptionsProbe) Complete(ctx context.Context, _ []Message, _ []ToolSpec) (modeldata.AssistantMessage, error) {
	route, _ := ModelRouteFromContext(ctx)
	m.routes = append(m.routes, route)
	m.caps = append(m.caps, ModelCallOptionsFromContext(ctx).MaxOutputTokens)
	return modeldata.AssistantMessage{Content: "done"}, nil
}

func TestRecordedModelSelectionUsesNativeGoResolver(t *testing.T) {
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, CreatedAt: 1, ID: "selection", CWD: "/tmp"}, &nativeTestJournal{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	probe := &modelOptionsProbe{}
	loop, err := NewNativeLoop(live, probe, "p", "initial", "prompt", nil, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetModelSelectionResolver(func(_ context.Context, route ModelRoute) (Model, error) { return probe, nil })
	for _, name := range []string{"first", "second"} {
		if err := live.SelectModel(sessionformat.ModelSelection{Provider: "p", Model: name}); err != nil {
			t.Fatal(err)
		}
		if _, err := loop.RunTextTurn(t.Context(), "hello"); err != nil {
			t.Fatal(err)
		}
	}
	if len(probe.routes) != 2 || probe.routes[0].Model != "first" || probe.routes[1].Model != "second" {
		t.Fatal(probe.routes)
	}
}
