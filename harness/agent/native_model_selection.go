package agent

import (
	"context"
	"errors"
)

type ModelSelectionResolver func(context.Context, ModelRoute) (Model, error)

func (a *NativeLoop) SetModelSelectionResolver(resolve ModelSelectionResolver) {
	a.resolveSelectedModel = resolve
}

func (a *NativeLoop) captureStepModel(ctx context.Context) (context.Context, error) {
	if a.resolveSelectedModel == nil {
		return ctx, nil
	}
	state, err := a.session.ModelSelection()
	if err != nil {
		return ctx, err
	}
	selected := state.Current()
	if selected == nil {
		return WithModelRoute(ctx, ModelRoute{Provider: a.provider, Model: a.modelID, ReasoningEffort: a.reasoningEffort}), nil
	}

	route := ModelRoute{Provider: selected.Provider, Model: selected.Model, ReasoningEffort: selected.ReasoningEffort}
	model := a.model
	changed := a.provider != route.Provider || a.modelID != route.Model || a.reasoningEffort != route.ReasoningEffort
	if a.resolveSelectedModel != nil && changed {
		model, err = a.resolveSelectedModel(ctx, route)
		if err != nil {
			return ctx, err
		}
		if model == nil {
			return ctx, errors.New("selected model is unavailable")
		}
	}
	if changed {
		a.requestLogged = false
	}
	a.model, a.provider, a.modelID, a.reasoningEffort = model, route.Provider, route.Model, route.ReasoningEffort
	// The same captured route reaches tools, prompt assembly, retries and the
	// real adapter. A later select affects only the next preparation boundary.
	return WithModelRoute(WithTurnModel(ctx, TurnModel{Model: model, Route: route}), route), nil
}
