package mcpconfig

import "encoding/json"

type ProbeOptions struct {
	TimeoutMS    *float64 `json:"timeoutMs,omitempty"`
	MaxPages     *int64   `json:"maxPages,omitempty"`
	MaxToolNames *int64   `json:"maxToolNames,omitempty"`
}

type CloseResult struct {
	CloseConfirmed bool   `json:"closeConfirmed"`
	CleanupError   string `json:"cleanupError,omitempty"`
}

type ProbeServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ProbeResult preserves the upstream connect/list union and independent
// cleanup result. It never reports Agent execution or persistent activation.
type ProbeResult struct {
	CloseResult
	OK         bool             `json:"ok"`
	DurationMS int64            `json:"durationMs"`
	ServerInfo *ProbeServerInfo `json:"serverInfo,omitempty"`
	ToolNames  []string         `json:"toolNames"`
	Truncated  bool             `json:"truncated"`
	Stage      string           `json:"stage,omitempty"`
	Message    string           `json:"message,omitempty"`
}

func (r ProbeResult) MarshalJSON() ([]byte, error) {
	if !r.OK {
		return json.Marshal(struct {
			CloseResult
			OK         bool   `json:"ok"`
			Stage      string `json:"stage"`
			Message    string `json:"message"`
			DurationMS int64  `json:"durationMs"`
		}{r.CloseResult, false, r.Stage, r.Message, r.DurationMS})
	}
	names := r.ToolNames
	if names == nil {
		names = []string{}
	}
	return json.Marshal(struct {
		CloseResult
		OK         bool             `json:"ok"`
		ServerInfo *ProbeServerInfo `json:"serverInfo,omitempty"`
		ToolNames  []string         `json:"toolNames"`
		Truncated  bool             `json:"truncated"`
		DurationMS int64            `json:"durationMs"`
	}{r.CloseResult, true, r.ServerInfo, names, r.Truncated, r.DurationMS})
}
