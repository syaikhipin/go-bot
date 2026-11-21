// Package proactivesettings is the small Controller/Host wire contract.
// Keep Agent assembly, tools and SQLite out of Controller's dependency tree.
package proactivesettings

const SettingsPath = "/v1/settings/proactive"
const InternalPath = "/internal/host/proactive"

type Settings struct {
	Enabled bool `json:"enabled"`
}

type Status struct {
	Enabled          bool   `json:"enabled"`
	Phase            string `json:"phase"`
	NextCheckAt      int64  `json:"nextCheckAt,omitempty"`
	NextCheckReason  string `json:"nextCheckReason,omitempty"`
	LastRunStartedAt int64  `json:"lastRunStartedAt,omitempty"`
	LastCompletedAt  int64  `json:"lastCompletedAt,omitempty"`
	Error            string `json:"error,omitempty"`
}
