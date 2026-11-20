package mcpclient

import (
	"context"
	"sync"
	"time"

	mcpconfig "go-bot/mcp"
)

type serverProbe struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type probeRun struct {
	call    context.Context
	entry   *serverEntry
	config  mcpconfig.Server
	status  mcpconfig.ServerStatus
	options ConnectionOptions
	finish  func()
}

// beginProbe snapshots the real entry and joins its cleanup fence. Both
// client-facing projections use this same ownership and cancellation path.
func (m *Manager) beginProbe(ctx context.Context, name string, timeout time.Duration) (probeRun, error) {
	if !mcpconfig.ValidServerName(name) {
		return probeRun{}, fail("MCP_INVALID_CONFIG", "Invalid MCP server name")
	}
	m.mu.Lock()
	entry := m.entries[name]
	if entry == nil || entry.ctx.Err() != nil || m.closed {
		m.mu.Unlock()
		return probeRun{}, fail("MCP_NOT_FOUND", "MCP server is not available for probing")
	}
	if entry.cleanupFailure != nil {
		m.mu.Unlock()
		return probeRun{}, fail("MCP_CLEANUP_INCOMPLETE", "MCP probe cleanup failed; restart the Host before replacing this server")
	}
	config := (mcpconfig.Settings{Servers: map[string]mcpconfig.Server{name: entry.config}}).Clone().Servers[name]
	result := mcpconfig.ServerStatus{Name: name, Source: entry.status.Source, Owner: entry.status.Owner, Missing: append([]string(nil), entry.status.Missing...), OAuthEnabled: entry.config.OAuth != nil}
	options := m.options.Connection
	options.Secrets = append([]string(nil), options.Secrets...)
	for secret := range m.secrets {
		options.Secrets = append(options.Secrets, secret)
	}
	call, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(entry.ctx, cancel)
	probe := &serverProbe{cancel: cancel, done: make(chan struct{})}
	if entry.probes == nil {
		entry.probes = make(map[*serverProbe]struct{})
	}
	entry.probes[probe] = struct{}{}
	m.mu.Unlock()
	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			stop()
			cancel()
			m.mu.Lock()
			delete(entry.probes, probe)
			close(probe.done)
			m.mu.Unlock()
		})
	}
	return probeRun{call: call, entry: entry, config: config, status: result, options: options, finish: finish}, nil
}

// Probe is the bundled clients' status projection. The Cordis contract is
// ProbeDetailed; neither operation publishes persistent tools or logs in.
func (m *Manager) Probe(ctx context.Context, name string) (mcpconfig.ServerStatus, error) {
	run, err := m.beginProbe(ctx, name, RequestTimeout)
	if err != nil {
		return mcpconfig.ServerStatus{}, err
	}
	call, entry, config, result, options, finish := run.call, run.entry, run.config, run.status, run.options, run.finish
	deferredCleanup := false
	defer func() {
		if !deferredCleanup {
			finish()
		}
	}()
	if len(result.Missing) > 0 {
		result.State = "waiting-credentials"
		return result, nil
	}
	if config.OAuth != nil {
		var err error
		options, err = m.connectionOptions(entry)
		if authorizationRequired(err) {
			result.State = "waiting-authorization"
			return result, nil
		}
		if err != nil {
			result.State, result.Error = "failed", m.redact(err.Error())
			return result, nil
		}
	}
	options.connectCleanup = func(err error) {
		if err != nil {
			m.retainCleanup(entry, mcpconfig.CloseResult{CloseConfirmed: false, CleanupError: m.redact(err.Error())})
		}
	}
	connection, err := Connect(call, name, config, options)
	if err != nil {
		if authorizationRequired(err) {
			result.State = "waiting-authorization"
			return result, nil
		}
		result.State, result.Error = "failed", m.redact(err.Error())
		return result, nil
	}
	discovery, discoverErr := connection.Discover(call)
	closeErr := connection.Close()
	if closeErr != nil {
		closed := false
		select {
		case <-connection.Closed():
			closed = true
		default:
		}
		m.retainCleanup(entry, mcpconfig.CloseResult{CloseConfirmed: closed, CleanupError: m.redact(closeErr.Error())})
		// Keep this probe in the entry's cleanup fence until the transport is
		// actually closed. A replacement must not overlap an orphan process.
		deferredCleanup = true
		go func() { <-connection.Closed(); finish() }()
		result.State, result.Error = "stopping", m.redact(closeErr.Error())
		return result, closeErr
	}
	if discoverErr != nil {
		result.State, result.Error = "failed", m.redact(discoverErr.Error())
		return result, nil
	}
	result.State, result.ToolCount, result.Resources, result.ProtocolVersion = "connected", len(discovery.Tools), discovery.Resources, discovery.ProtocolVersion
	if discovery.ServerInfo != nil {
		result.ServerName = discovery.ServerInfo.Name
	}
	return result, nil
}
