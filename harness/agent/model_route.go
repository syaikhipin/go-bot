package agent

import (
	"context"
	"errors"

	"go-harness/internal/modeldata"
)

type modelRouteKey struct{}
type modelCallOptionsKey struct{}

// ModelCallOptions describes an auxiliary request without changing the
// conversation route. Zero values keep an ordinary Agent step unchanged.
type ModelCallOptions struct {
	Purpose         string `json:"purpose,omitempty"`
	MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
}

func WithModelCallOptions(ctx context.Context, options ModelCallOptions) context.Context {
	return context.WithValue(ctx, modelCallOptionsKey{}, options)
}

func ModelCallOptionsFromContext(ctx context.Context) ModelCallOptions {
	options, _ := ctx.Value(modelCallOptionsKey{}).(ModelCallOptions)
	return options
}

type ModelRoute struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

func WithModelRoute(ctx context.Context, route ModelRoute) context.Context {
	return context.WithValue(ctx, modelRouteKey{}, route)
}

func ModelRouteFromContext(ctx context.Context) (ModelRoute, bool) {
	route, ok := ctx.Value(modelRouteKey{}).(ModelRoute)
	return route, ok && route.Provider != "" && route.Model != ""
}

// RoutedModel changes only one child activation's model route. The parent and
// sibling Agents keep their own model settings and request headers.
type RoutedModel struct {
	Base  Model
	Route ModelRoute
}

func (m RoutedModel) Complete(ctx context.Context, messages []Message, specs []ToolSpec) (modeldata.AssistantMessage, error) {
	if m.Base == nil || m.Route.Provider == "" || m.Route.Model == "" {
		return modeldata.AssistantMessage{}, errors.New("model route is unavailable")
	}
	return m.Base.Complete(WithModelRoute(ctx, m.Route), messages, specs)
}

func (m RoutedModel) CheckRoute(ctx context.Context, route ModelRoute) error {
	checker, ok := m.Base.(interface {
		CheckRoute(context.Context, ModelRoute) error
	})
	if !ok {
		return errors.New("model route preflight is unavailable")
	}
	return checker.CheckRoute(ctx, route)
}

// TurnModel carries one immutable model configuration through a turn.
type TurnModel struct {
	Model Model
	Route ModelRoute
}

type turnModelKey struct{}

func TurnModelFromContext(ctx context.Context) (TurnModel, bool) {
	model, ok := ctx.Value(turnModelKey{}).(TurnModel)
	return model, ok
}

func WithTurnModel(ctx context.Context, model TurnModel) context.Context {
	return context.WithValue(ctx, turnModelKey{}, model)
}
