package mcpconfig

type ServerStatus struct {
	Name            string              `json:"name"`
	Source          string              `json:"source"`
	Owner           string              `json:"owner,omitempty"`
	State           string              `json:"state"`
	Error           string              `json:"error,omitempty"`
	Missing         []string            `json:"missing,omitempty"`
	ToolCount       int                 `json:"toolCount"`
	Resources       bool                `json:"resources"`
	ProtocolVersion string              `json:"protocolVersion,omitempty"`
	ServerName      string              `json:"serverName,omitempty"`
	OAuthEnabled    bool                `json:"oauthEnabled"`
	Authorization   *OAuthAuthorization `json:"authorization,omitempty"`
}

type OAuthAuthorization struct {
	ID        string `json:"id,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

type OAuthCallback struct {
	State  string `json:"state"`
	Code   string `json:"code,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Receipt acknowledges transport admission; it is not an authorization outcome.
type OAuthCallbackReceipt struct {
	State string `json:"state"`
}

type Status struct {
	Servers []ServerStatus `json:"servers"`
	Error   string         `json:"error,omitempty"`
}

// ManagedServerView is the plugin-facing list/watch shape. The connection is
// still owned by Go; Node receives only these public status values.
type ManagedServerView struct {
	Name          string              `json:"name"`
	Configured    bool                `json:"configured"`
	Status        ManagedServerStatus `json:"status"`
	Authorization *OAuthAttemptView   `json:"authorization,omitempty"`
}

type View struct {
	Status Status `json:"status"`
}
