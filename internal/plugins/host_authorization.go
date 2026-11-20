package plugins

import (
	"context"
	"encoding/json"
	"strings"

	"go-bot/internal/authorization"
	"go-bot/internal/credentials"
)

type hostAuthorizationDefinition struct {
	Key     string                 `json:"key"`
	Title   string                 `json:"title"`
	Methods []authorization.Method `json:"methods"`
}

func validateHostAuthorization(row *hostContribution) error {
	var definition hostAuthorizationDefinition
	if err := decodeHostJSON(row.Descriptor, &definition); err != nil {
		return err
	}
	if _, err := credentialAddress(row.Owner, definition.Key); err != nil {
		return err
	}
	if strings.TrimSpace(definition.Title) == "" || len(definition.Methods) == 0 {
		return failure("INVALID_ARGUMENT", "Authorization needs a title and methods")
	}
	for _, method := range row.Callbacks {
		if method == "run" {
			return nil
		}
	}
	return failure("INVALID_ARGUMENT", "Authorization run callback is required")
}

func (r *hostBridge) registerAuthorizationResource(row *hostContribution) (func(context.Context) error, error) {
	if r.config.Authorization == nil {
		return nil, failure("UNAVAILABLE", "Go authorization service is not configured")
	}
	var definition hostAuthorizationDefinition
	_ = json.Unmarshal(row.Descriptor, &definition)
	key, _ := credentialAddress(row.Owner, definition.Key)
	flow := authorization.Flow{Key: key, Label: definition.Title, Methods: definition.Methods, Run: func(session *authorization.Session) error {
		return r.callback(session.Context, row, "run", map[string]string{"id": session.ID, "method": session.Method}, nil, nil)
	}}
	for _, method := range row.Callbacks {
		switch method {
		case "inspect":
			flow.Inspect = func(ctx context.Context) (authorization.AccountState, error) {
				var state authorization.AccountState
				err := r.callback(ctx, row, "inspect", struct{}{}, nil, &state)
				return state, err
			}
		case "disconnect":
			flow.Disconnect = func(ctx context.Context) error { return r.callback(ctx, row, "disconnect", struct{}{}, nil, nil) }
		}
	}
	dispose, err := r.config.Authorization.Register(flow)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		dispose()
		_, err := r.config.Authorization.CancelAndWait(ctx, key)
		return err
	}, nil
}

func (r *hostBridge) authorizationRequest(ctx context.Context, owner, method string, raw json.RawMessage, mutation string) (any, error) {
	service := r.config.Authorization
	if service == nil {
		return nil, failure("UNAVAILABLE", "Go authorization service is not configured")
	}
	var input struct {
		Key    string               `json:"key"`
		ID     string               `json:"id"`
		State  string               `json:"state"`
		Notice authorization.Notice `json:"notice"`
		Prompt authorization.Prompt `json:"prompt"`
		Record *credentials.Record  `json:"record"`
	}
	if err := decodeHostJSON(raw, &input); err != nil {
		return nil, err
	}
	if method == "list" {
		result := []map[string]any{}
		for _, entry := range service.List() {
			if key, ok := strings.CutPrefix(entry.Key, owner+"/"); ok {
				phase := "idle"
				if entry.InFlight {
					phase = "running"
				}
				result = append(result, map[string]any{"key": key, "title": entry.Label, "methods": entry.Methods, "phase": phase})
			}
		}
		return result, nil
	}
	if method == "cancel" {
		key, err := credentialAddress(owner, input.Key)
		if err != nil {
			return nil, err
		}
		service.Cancel(key)
		return nil, nil
	}
	session := service.Session(input.ID)
	if session == nil || !strings.HasPrefix(session.Key, owner+"/") {
		return nil, failure("UNAVAILABLE", "Authorization attempt is no longer active")
	}
	switch method {
	case "notify":
		session.Notify(ctx, input.Notice)
		return nil, nil
	case "prompt":
		return session.Prompt(ctx, input.Prompt)
	case "callback":
		return session.Callback(ctx, input.State)
	case "waitCallback":
		return session.WaitCallback(ctx)
	case "commit":
		if mutation != "" {
			return nil, failure("CONFLICT", "A credential mutation cannot recursively commit an authorization")
		}
		return nil, session.Commit(input.Record)
	default:
		return nil, failure("NOT_FOUND", "Unknown authorization method")
	}
}
