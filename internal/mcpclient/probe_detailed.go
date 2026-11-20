package mcpclient

import (
	"context"
	"math"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	mcpconfig "go-bot/mcp"
)

type probeSpec struct {
	timeout            time.Duration
	maxPages, maxNames int64
}

func resolveProbeOptions(options mcpconfig.ProbeOptions) (probeSpec, error) {
	spec := probeSpec{timeout: 10 * time.Second, maxPages: 16, maxNames: 512}
	if options.TimeoutMS != nil {
		value := *options.TimeoutMS
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return spec, fail("MCP_INVALID_PROBE_OPTIONS", "timeoutMs must be a positive finite number")
		}
		if value >= float64(math.MaxInt64)/float64(time.Millisecond) {
			spec.timeout = time.Duration(math.MaxInt64)
		} else {
			spec.timeout = time.Duration(value * float64(time.Millisecond))
		}
	}
	for _, field := range []struct {
		name   string
		input  *int64
		target *int64
	}{{"maxPages", options.MaxPages, &spec.maxPages}, {"maxToolNames", options.MaxToolNames, &spec.maxNames}} {
		if field.input == nil {
			continue
		}
		if *field.input < 1 || *field.input > 9007199254740991 {
			return spec, fail("MCP_INVALID_PROBE_OPTIONS", field.name+" must be a positive safe integer")
		}
		*field.target = *field.input
	}
	return spec, nil
}

// ProbeDetailed implements the original plugin-facing probe contract. It
// lists raw advertised names, without publishing or compiling model tools.
// Expected connection/list/cancellation failures are values, not RPC errors.
func (m *Manager) ProbeDetailed(ctx context.Context, name string, options mcpconfig.ProbeOptions) (mcpconfig.ProbeResult, error) {
	spec, err := resolveProbeOptions(options)
	if err != nil {
		return mcpconfig.ProbeResult{}, err
	}
	started := time.Now()
	run, err := m.beginProbe(ctx, name, spec.timeout)
	if err != nil {
		return mcpconfig.ProbeResult{}, err
	}
	deferredCleanup := false
	defer func() {
		if !deferredCleanup {
			run.finish()
		}
	}()
	result := mcpconfig.ProbeResult{CloseResult: mcpconfig.CloseResult{CloseConfirmed: true}, Stage: "connect", ToolNames: []string{}}
	failure := func(message string) (mcpconfig.ProbeResult, error) {
		result.OK = false
		result.Message = m.redact(message)
		if ctx.Err() != nil || run.entry.ctx.Err() != nil {
			result.Message = "probe aborted"
		} else if run.call.Err() != nil {
			result.Message = "probe timed out"
		}
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}
	if run.call.Err() != nil {
		return failure(run.call.Err().Error())
	}
	if len(run.status.Missing) > 0 {
		return failure("Missing credentials: " + strings.Join(run.status.Missing, ", "))
	}
	connectionOptions := run.options
	if run.config.OAuth != nil {
		connectionOptions, err = m.connectionOptions(run.entry)
		if err != nil {
			return failure(err.Error())
		}
	}
	// The whole probe budget owns negotiation; the live instance's startup
	// timeout does not shorten this independent operation.
	connectionOptions.StartupTimeout = spec.timeout
	connectionOptions.connectCleanup = func(cleanupErr error) {
		if cleanupErr != nil {
			result.CloseConfirmed = false
			result.CleanupError = m.redact(cleanupErr.Error())
		}
	}
	connection, err := Connect(run.call, name, run.config, connectionOptions)
	if err != nil {
		if result.CleanupError != "" {
			// Failed negotiation returned no connection handle whose closure can
			// later be confirmed. Retain the namespace instead of retrying it.
			m.retainCleanup(run.entry, result.CloseResult)
		}
		return failure(err.Error())
	}
	result.Stage = "list"
	info := connection.Session.InitializeResult()
	if info.ServerInfo != nil {
		result.ServerInfo = &mcpconfig.ProbeServerInfo{Name: info.ServerInfo.Name, Version: info.ServerInfo.Version}
	}
	var listErr error
	if info.Capabilities != nil && info.Capabilities.Tools != nil {
		cursor := ""
		for page := int64(0); page < spec.maxPages; page++ {
			response, err := connection.Session.ListTools(run.call, &sdk.ListToolsParams{Cursor: cursor})
			if err != nil {
				listErr = connection.requestError(run.call, err)
				break
			}
			for _, tool := range response.Tools {
				if int64(len(result.ToolNames)) >= spec.maxNames {
					result.Truncated = true
					break
				}
				if tool == nil {
					listErr = fail("MCP_INVALID_LIST", "MCP probe received a null tool")
					break
				}
				result.ToolNames = append(result.ToolNames, tool.Name)
			}
			if listErr != nil {
				break
			}
			cursor = response.NextCursor
			if cursor == "" || result.Truncated {
				break
			}
			if page == spec.maxPages-1 {
				result.Truncated = true
			}
		}
	}
	if listErr == nil && run.call.Err() != nil {
		listErr = run.call.Err()
	}
	if listErr != nil {
		result, _ = failure(listErr.Error())
	} else {
		result.OK = true
		result.DurationMS = time.Since(started).Milliseconds()
	}
	// Capture the outcome before closing: duration excludes cleanup and a
	// cleanup failure must not turn a successful listing into a list failure.
	closeErr := connection.Close()
	select {
	case <-connection.Closed():
		result.CloseConfirmed = true
	default:
		result.CloseConfirmed = false
		deferredCleanup = true
		go func() { <-connection.Closed(); run.finish() }()
	}
	if closeErr != nil {
		result.CleanupError = m.redact(closeErr.Error())
		m.retainCleanup(run.entry, result.CloseResult)
	}
	return result, nil
}

func (m *Manager) retainCleanup(entry *serverEntry, result mcpconfig.CloseResult) {
	m.mu.Lock()
	if m.entries[entry.name] == entry && entry.cleanupFailure == nil {
		copy := result
		entry.cleanupFailure = &copy
		entry.status.State, entry.status.Error = "failed", result.CleanupError
	}
	m.mu.Unlock()
	m.changed()
}
