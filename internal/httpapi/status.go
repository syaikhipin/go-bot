package httpapi

import "errors"

var ErrNotReady = errors.New("Agent is not ready")

type ActiveStatus struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
	LastError string `json:"lastError,omitempty"`
}
