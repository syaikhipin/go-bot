package mcpclient

import (
	"strings"

	mcpconfig "go-bot/mcp"
)

func (c *Connection) authorizationFailure() error {
	if detail := c.authorizationError.Load(); detail != nil {
		return detail
	}
	return authorizationFailure("rejected", "Reconnect this MCP server with OAuth")
}

func transportClosed(connection *Connection) bool {
	if connection == nil {
		return false
	}
	select {
	case <-connection.Closed():
		return true
	default:
		return false
	}
}

func (m *Manager) closeGeneration(entry *serverEntry, connection *Connection) error {
	err := connection.Close()
	if err != nil {
		m.retainCleanup(entry, mcpconfig.CloseResult{CloseConfirmed: transportClosed(connection), CleanupError: m.redact(err.Error())})
	}
	return err
}

// Called under m.mu. The values are recorded by the actual supervisor, not
// inferred from Node timers or a public diagnostic's wording.
func (m *Manager) runtimeStatusLocked(entry *serverEntry) mcpconfig.ManagedServerStatus {
	if entry.cleanupFailure != nil {
		result := *entry.cleanupFailure
		return mcpconfig.ManagedServerStatus{State: "failed", Reason: "cleanup", Result: &result}
	}
	state := entry.status.State
	if entry.ctx.Err() != nil {
		confirmed := false
		select {
		case <-entry.done:
			confirmed = entry.connection == nil || transportClosed(entry.connection)
		default:
		}
		return mcpconfig.ManagedServerStatus{State: "closed", Result: &mcpconfig.CloseResult{CloseConfirmed: confirmed}}
	}
	status := mcpconfig.ManagedServerStatus{State: state, LastError: entry.status.Error, Attempt: entry.attempt, MaxAttempts: 10, NextRetryAtMS: entry.nextRetryAt, ConnectedAt: entry.connectedAt, ToolCount: entry.status.ToolCount}
	switch state {
	case "waiting-credentials":
		status.LastError = "Missing MCP credentials: " + strings.Join(entry.status.Missing, ", ")
	case "waiting-authorization":
		status.Reason = entry.authorizationReason
		if status.Reason == "" {
			status.Reason = "rejected"
		}
	case "failed":
		status.Reason = "gave-up"
	}
	return status
}
