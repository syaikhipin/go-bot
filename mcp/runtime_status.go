package mcpconfig

import (
	"encoding/json"
	"fmt"
)

// ManagedServerStatus is the plugin-facing upstream union. HTTP settings keep
// the separate ServerStatus projection used by this project's own clients.
type ManagedServerStatus struct {
	State         string
	Reason        string
	LastError     string
	Result        *CloseResult
	Attempt       int
	MaxAttempts   int
	ToolCount     int
	ConnectedAt   int64
	NextRetryAtMS int64
}

func (s ManagedServerStatus) MarshalJSON() ([]byte, error) {
	value := map[string]any{"state": s.State}
	switch s.State {
	case "disabled":
	case "closed":
		if s.Result == nil {
			return nil, fmt.Errorf("closed MCP status requires a cleanup result")
		}
		value["result"] = s.Result
	case "waiting-credentials":
		value["lastError"] = s.LastError
	case "waiting-authorization":
		value["reason"], value["lastError"] = s.Reason, s.LastError
	case "connecting":
		value["attempt"] = s.Attempt
	case "connected":
		value["toolCount"], value["connectedAt"] = s.ToolCount, s.ConnectedAt
	case "reconnecting":
		value["attempt"], value["maxAttempts"], value["nextRetryAtMs"], value["lastError"] = s.Attempt, s.MaxAttempts, s.NextRetryAtMS, s.LastError
	case "failed":
		value["reason"] = s.Reason
		if s.Reason == "cleanup" {
			if s.Result == nil {
				return nil, fmt.Errorf("failed MCP cleanup status requires a cleanup result")
			}
			value["result"] = s.Result
		} else if s.LastError != "" || s.Reason == "load" {
			value["lastError"] = s.LastError
		}
	default:
		return nil, fmt.Errorf("unknown MCP runtime state %q", s.State)
	}
	return json.Marshal(value)
}

func (s *ManagedServerStatus) UnmarshalJSON(data []byte) error {
	var value struct {
		State         string       `json:"state"`
		Reason        string       `json:"reason"`
		LastError     string       `json:"lastError"`
		Result        *CloseResult `json:"result"`
		Attempt       int          `json:"attempt"`
		MaxAttempts   int          `json:"maxAttempts"`
		ToolCount     int          `json:"toolCount"`
		ConnectedAt   int64        `json:"connectedAt"`
		NextRetryAtMS int64        `json:"nextRetryAtMs"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*s = ManagedServerStatus{State: value.State, Reason: value.Reason, LastError: value.LastError, Result: value.Result, Attempt: value.Attempt, MaxAttempts: value.MaxAttempts, ToolCount: value.ToolCount, ConnectedAt: value.ConnectedAt, NextRetryAtMS: value.NextRetryAtMS}
	return nil
}

type OAuthAttemptView struct {
	ID               string `json:"id"`
	Phase            string `json:"phase"`
	AuthorizationURL string `json:"authorizationUrl,omitempty"`
	LastError        string `json:"lastError,omitempty"`
}

func (a OAuthAuthorization) AttemptView() *OAuthAttemptView {
	if a.ID == "" {
		return nil
	}
	view := &OAuthAttemptView{ID: a.ID, Phase: a.Phase}
	if a.Phase == "waiting-browser" {
		view.AuthorizationURL = a.URL
	}
	if a.Phase == "failed" {
		view.LastError = a.Error
	}
	return view
}
