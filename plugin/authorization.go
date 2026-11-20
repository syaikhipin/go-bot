package plugin

const AuthorizationPath = "/v1/host/extensions/authorization"
const AuthorizationCallbackPath = "/v1/plugins/authorization/callback/"
const AuthorizationCallbackHeader = "X-Go-Bot-Authorization-Callback"

type AuthorizationStart struct {
	Key    string `json:"key"`
	Method string `json:"method,omitempty"`
}

type AuthorizationAnswer struct {
	PromptID string  `json:"promptId"`
	Answer   *string `json:"answer,omitempty"`
	Decline  bool    `json:"decline,omitempty"`
}
