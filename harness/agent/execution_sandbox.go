package agent

import "path/filepath"

// ExecutionSandboxPolicy carries the current Go session policy across a
// capability boundary. It is not another permission store or Session projection.
// The filesystem provider accepts the same per-call shape as upstream.
type ExecutionSandboxPolicy struct {
	Mode          string `json:"mode"`
	WorkspaceRoot string `json:"workspaceRoot"`
	SessionID     string `json:"sessionId,omitempty"`
}

// SetSandboxModeSource binds the existing permission owner at Host assembly.
// The source is read at each operation boundary; the Agent keeps no mode copy.
func (a *NativeLoop) SetSandboxModeSource(source func() string) { a.sandboxMode = source }

func (a *nativeTools) executionSandboxPolicy() *ExecutionSandboxPolicy {
	header := a.header()
	if !filepath.IsAbs(header.CWD) {
		return nil
	}
	mode := "danger-full-access"
	if a.sandboxMode != nil {
		mode = a.sandboxMode()
	}
	return &ExecutionSandboxPolicy{Mode: mode, WorkspaceRoot: header.CWD, SessionID: header.ID}
}
