package mcpclient

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	authservice "go-bot/internal/authorization"
	"go-bot/internal/credentials"
	mcpconfig "go-bot/mcp"
)

type ManagerOptions struct {
	Authorization        *authservice.Service
	Credentials          *credentials.Store
	Images               ImageWriter
	CWD                  string
	Connection           ConnectionOptions
	Changed              func()
	OAuthCredentialsPath string
}

type serverEntry struct {
	name, owner         string
	registered          bool // Plugin registration intent, independent of connection lifetime.
	config              mcpconfig.Server
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	connection          *Connection
	generationCancel    context.CancelFunc
	generation          context.Context
	discovery           Discovery
	tools               []*Tool
	status              mcpconfig.ServerStatus
	probes              map[*serverProbe]struct{}
	cleanupFailure      *mcpconfig.CloseResult
	oauthBinding        string
	attempt             int
	nextRetryAt         int64
	connectedAt         int64
	publishedConnection *Connection
	authorizationReason string
}

// Manager owns both user and plugin connections in the same Host namespace.
// Configuration requests serialize; live calls, discovery and notifications
// do not hold the management gate. Only connection supervision retries, never
// a model's tool operation or a Controller configuration request.
type Manager struct {
	ctx             context.Context
	cancel          context.CancelFunc
	options         ManagerOptions
	gate            chan struct{}
	mu              sync.RWMutex
	entries         map[string]*serverEntry
	desired         mcpconfig.Settings
	secrets         map[string]bool
	changes         chan struct{}
	lastError       string
	closed          bool
	oauthStore      *oauthGrantStore
	oauthStoreError error
	ownsCredentials bool
	authorizations  map[string]*oauthAttempt
	oauthCallback   string // Last verified Controller callback; Host memory only.
}

func NewManager(ctx context.Context, options ManagerOptions) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: ctx, cancel: cancel, options: options, gate: make(chan struct{}, 1), entries: map[string]*serverEntry{}, secrets: map[string]bool{}, changes: make(chan struct{}), authorizations: map[string]*oauthAttempt{}}
	path := options.OAuthCredentialsPath
	if path == "" && options.CWD != "" {
		path = filepath.Join(options.CWD, ".dsh", credentials.Filename)
	}
	if options.Credentials != nil {
		m.oauthStore = &oauthGrantStore{credentials: options.Credentials}
	} else if path != "" {
		m.oauthStore, m.oauthStoreError = newOAuthGrantStore(path)
		m.ownsCredentials = m.oauthStore != nil
	}

	if m.options.Authorization == nil && m.oauthStore != nil {
		m.options.Authorization = authservice.New(m.oauthStore.credentials)
	}
	return m
}

func (m *Manager) enter(ctx context.Context) (func(), error) {
	select {
	case m.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, m.ctx.Err()
	}
	if m.ctx.Err() != nil {
		<-m.gate
		return nil, m.ctx.Err()
	}
	return func() { <-m.gate }, nil
}
func (m *Manager) changed() {
	m.mu.Lock()
	close(m.changes)
	m.changes = make(chan struct{})
	m.mu.Unlock()
	if m.options.Changed != nil {
		m.options.Changed()
	}
}
func (m *Manager) Changes() (<-chan struct{}, <-chan struct{}) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.changes, m.ctx.Done()
}
func (m *Manager) redact(message string) string {
	if m.options.Credentials != nil {
		message = m.options.Credentials.Redact(message)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for secret := range m.secrets {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	if len(message) > 4096 {
		message = message[:4096] + "…"
	}
	return message
}
func (m *Manager) state(entry *serverEntry, state string, err error) {
	m.stateUpdate(entry, state, err, nil)
}

func (m *Manager) stateUpdate(entry *serverEntry, state string, err error, update func()) {
	message := ""
	if err != nil {
		message = m.redact(err.Error())
	}
	m.mu.Lock()
	if update != nil {
		update()
	}
	entry.status.State, entry.status.Error = state, message
	if state == "waiting-authorization" {
		var detail *Error
		if errors.As(err, &detail) {
			entry.authorizationReason = detail.AuthorizationReason
		}
	}
	if state == "stopping" && entry.cleanupFailure == nil {
		entry.cleanupFailure = &mcpconfig.CloseResult{CloseConfirmed: transportClosed(entry.connection), CleanupError: message}
	}
	if entry.cleanupFailure != nil {
		entry.status.State, entry.status.Error = "failed", entry.cleanupFailure.CleanupError
	}
	m.mu.Unlock()
	m.changed()
}
func (m *Manager) Status() mcpconfig.Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.statusLocked()
}

func (m *Manager) statusLocked() mcpconfig.Status {
	status := mcpconfig.Status{Servers: []mcpconfig.ServerStatus{}, Error: m.lastError}
	for _, entry := range m.entries {
		value := entry.status
		value.OAuthEnabled = entry.config.OAuth != nil
		if value.Authorization != nil {
			copy := *value.Authorization
			value.Authorization = &copy
		}
		value.Missing = append([]string(nil), value.Missing...)
		status.Servers = append(status.Servers, value)
	}
	sort.Slice(status.Servers, func(i, j int) bool { return status.Servers[i].Name < status.Servers[j].Name })
	return status
}

func (m *Manager) ListViews() []mcpconfig.ManagedServerView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := []mcpconfig.ManagedServerView{}
	seen := map[string]bool{}
	for _, status := range m.statusLocked().Servers {
		entry := m.entries[status.Name]
		_, configured := m.desired.Servers[status.Name]
		if entry.owner != "" {
			configured = entry.registered
		}
		seen[status.Name] = true
		var authorization *mcpconfig.OAuthAttemptView
		if status.Authorization != nil {
			authorization = status.Authorization.AttemptView()
		}
		views = append(views, mcpconfig.ManagedServerView{Name: status.Name, Configured: configured, Status: m.runtimeStatusLocked(entry), Authorization: authorization})
	}
	// Configuration intent is visible before sequential connection setup has
	// reached each server, just as upstream list() reads entries and records.
	for name, config := range m.desired.Servers {
		if seen[name] {
			continue
		}
		state := "connecting"
		if !config.IsEnabled() {
			state = "disabled"
		}
		status := mcpconfig.ManagedServerStatus{State: state}
		if m.ctx.Err() != nil {
			status = mcpconfig.ManagedServerStatus{State: "closed", Result: &mcpconfig.CloseResult{CloseConfirmed: true}}
		}
		views = append(views, mcpconfig.ManagedServerView{Name: name, Configured: true, Status: status})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

func (m *Manager) Apply(ctx context.Context, settings mcpconfig.Settings) (mcpconfig.Status, error) {
	if err := settings.Validate(); err != nil {
		return m.Status(), err
	}
	leave, err := m.enter(ctx)
	if err != nil {
		return m.Status(), err
	}
	defer leave()
	settings = settings.Clone()
	m.mu.Lock()
	for name := range settings.Servers {
		if entry := m.entries[name]; entry != nil && entry.owner != "" {
			m.mu.Unlock()
			return m.Status(), fail("MCP_NAME_CONFLICT", "MCP server name is owned by a plugin: "+name)
		}
	}
	m.desired = settings
	for _, secret := range settings.Credentials {
		if secret != "" {
			m.secrets[secret] = true
		}
	}
	var remove []string
	for name, entry := range m.entries {
		if entry.owner == "" {
			if _, ok := settings.Servers[name]; !ok {
				remove = append(remove, name)
			}
		}
	}
	m.mu.Unlock()
	for _, name := range remove {
		if err = m.remove(ctx, name, ""); err != nil {
			return m.Status(), err
		}
	}
	var names []string
	for name := range settings.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	var combined, unresolved error
	for _, name := range names {
		resolved, missing, err := settings.Resolve(name)
		if err != nil {
			combined = errors.Join(combined, err)
			unresolved = errors.Join(unresolved, err)
			continue
		}
		if resolved.CWD == "" && resolved.Transport == "stdio" {
			resolved.CWD = m.options.CWD
		}
		if err = m.upsert(ctx, name, "", resolved, missing); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	message := ""
	// Connection failures already belong to each live server's status. Keeping
	// them as a fleet error would survive recovery and disable unrelated servers
	// in clients. Only configuration that could not become an entry stays here.
	if unresolved != nil {
		message = m.redact(unresolved.Error())
	}
	m.mu.Lock()
	m.lastError = message
	m.mu.Unlock()
	m.changed()
	return m.Status(), combined
}

func (m *Manager) upsert(ctx context.Context, name, owner string, config mcpconfig.Server, missing []string) error {
	binding := m.oauthConfigBinding(name, owner, config)
	m.mu.RLock()
	old := m.entries[name]
	m.mu.RUnlock()
	if old != nil {
		if old.owner != owner {
			return fail("MCP_NAME_CONFLICT", "MCP server name is already registered: "+name)
		}
		m.mu.RLock()
		same := reflect.DeepEqual(old.config, config) && reflect.DeepEqual(old.status.Missing, missing) && old.oauthBinding == binding
		state := old.status.State
		m.mu.RUnlock()
		if same && state != "failed" && state != "stopping" {
			return nil
		}
		if err := m.remove(ctx, name, owner); err != nil {
			return err
		}
	}
	lifetime, cancel := context.WithCancel(m.ctx)
	entry := &serverEntry{name: name, owner: owner, registered: owner != "", config: config, ctx: lifetime, cancel: cancel, done: make(chan struct{}), oauthBinding: binding}
	entry.status = mcpconfig.ServerStatus{Name: name, State: "connecting", Source: "user", Missing: append([]string(nil), missing...)}
	if owner != "" {
		entry.status.Source, entry.status.Owner = "plugin", owner
	}
	m.mu.Lock()
	for _, secret := range config.SecretValues() {
		m.secrets[secret] = true
	}
	m.entries[name] = entry
	m.mu.Unlock()
	if !config.IsEnabled() {
		close(entry.done)
		m.state(entry, "disabled", nil)
		return nil
	}
	if len(missing) > 0 {
		close(entry.done)
		m.state(entry, "waiting-credentials", nil)
		return nil
	}
	m.changed()
	ready := make(chan error, 1)
	go m.supervise(entry, ctx, ready)
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		entry.cancel()
		select {
		case <-entry.done:
		case <-time.After(CloseTimeout):
		}
		m.state(entry, "failed", ctx.Err())
		return ctx.Err()
	}
}

func (m *Manager) supervise(entry *serverEntry, first context.Context, ready chan<- error) {
	defer close(entry.done)
	firstAttempt := true
	attempts := 0
	notifyReady := func(err error) {
		if !firstAttempt {
			return
		}
		if authorizationRequired(err) {
			ready <- nil
		} else {
			ready <- err
		}
		firstAttempt = false
	}
	for {
		if entry.ctx.Err() != nil {
			return
		}
		m.stateUpdate(entry, "connecting", nil, func() { entry.attempt = attempts; entry.nextRetryAt = 0 })
		call := entry.ctx
		if firstAttempt {
			call = first
		}
		connectCtx, done := context.WithCancel(call)
		stop := context.AfterFunc(entry.ctx, done)
		options, err := m.connectionOptions(entry)
		var connection *Connection
		if err == nil {
			previous := options.connectCleanup
			options.connectCleanup = func(cleanupErr error) {
				if previous != nil {
					previous(cleanupErr)
				}
				if cleanupErr != nil {
					m.retainCleanup(entry, mcpconfig.CloseResult{CloseConfirmed: false, CleanupError: m.redact(cleanupErr.Error())})
				}
			}
			connection, err = Connect(connectCtx, entry.name, entry.config, options)
		}
		var discovery Discovery
		if err == nil {
			m.mu.Lock()
			entry.connection = connection
			m.mu.Unlock()
			discovery, err = connection.Discover(connectCtx)
		}
		stop()
		done()
		if err == nil && entry.ctx.Err() == nil {
			m.publish(entry, connection, discovery)
			attempts = 0
		} else {
			if connection != nil {
				_ = m.closeGeneration(entry, connection)
			}
			if err == nil {
				err = entry.ctx.Err()
			}
			if authorizationRequired(err) {
				m.state(entry, "waiting-authorization", err)
			}
		}
		if err == nil || authorizationRequired(err) {
			notifyReady(err)
		}
		if entry.ctx.Err() != nil {
			notifyReady(err)
			return
		}
		m.mu.RLock()
		cleanupFailed := entry.cleanupFailure != nil
		m.mu.RUnlock()
		if cleanupFailed {
			notifyReady(err)
			return
		}
		if authorizationRequired(err) {
			return
		}
		if err == nil {
			for err == nil {
				select {
				case <-entry.ctx.Done():
					m.withdraw(entry)
					_ = m.closeGeneration(entry, connection)
					return
				case <-connection.Disconnected():
					if connection.needsAuthorization.Load() {
						err = connection.authorizationFailure()
					} else {
						err = fail("MCP_SERVER_UNAVAILABLE", "MCP server disconnected")
					}
				case <-connection.Changes:
					var next Discovery
					next, err = connection.Discover(entry.ctx)
					if err == nil {
						m.publish(entry, connection, next)
					}
				}
			}
			m.withdraw(entry)
			if authorizationRequired(err) {
				m.state(entry, "waiting-authorization", err)
			}
			if closeErr := m.closeGeneration(entry, connection); closeErr != nil {
				return
			}
		}
		if authorizationRequired(err) {
			return
		}
		if attempts >= 10 {
			m.state(entry, "failed", err)
			notifyReady(err)
			return
		}
		delay := 500 * time.Millisecond << attempts
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		attempts++
		m.stateUpdate(entry, "reconnecting", err, func() { entry.attempt = attempts; entry.nextRetryAt = time.Now().Add(delay).UnixMilli() })
		// Publish the first failure before Apply/Register returns it. Otherwise
		// its status snapshot can still say connecting with no diagnostic.
		notifyReady(err)
		timer := time.NewTimer(delay)
		select {
		case <-entry.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) publish(entry *serverEntry, connection *Connection, discovery Discovery) {
	lifetime, cancel := context.WithCancel(entry.ctx)
	tools := make([]*Tool, 0, len(discovery.Tools))
	for _, definition := range discovery.Tools {
		tools = append(tools, NewTool(connection, definition, ToolOptions{Images: m.options.Images, Lifetime: lifetime}))
	}
	m.mu.Lock()
	if entry.ctx.Err() != nil || m.entries[entry.name] != entry {
		m.mu.Unlock()
		cancel()
		return
	}
	if entry.generationCancel != nil {
		entry.generationCancel()
	}
	if entry.publishedConnection != connection {
		entry.connectedAt = time.Now().UnixMilli()
		entry.publishedConnection = connection
	}
	entry.connection, entry.generationCancel, entry.generation, entry.discovery, entry.tools = connection, cancel, lifetime, discovery, tools
	entry.status.State, entry.status.Error = "connected", ""
	if entry.cleanupFailure != nil {
		entry.status.State, entry.status.Error = "failed", entry.cleanupFailure.CleanupError
	}
	entry.status.ToolCount, entry.status.Resources, entry.status.ProtocolVersion = len(tools), discovery.Resources, discovery.ProtocolVersion
	if discovery.ServerInfo != nil {
		entry.status.ServerName = discovery.ServerInfo.Name
	}
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) withdraw(entry *serverEntry) {
	m.mu.Lock()
	if entry.generationCancel != nil {
		entry.generationCancel()
	}
	m.mu.Unlock()
}

func (m *Manager) remove(ctx context.Context, name, owner string) error {
	m.mu.RLock()
	entry := m.entries[name]
	cleanupFailed := entry != nil && entry.cleanupFailure != nil
	closing := m.closed
	m.mu.RUnlock()
	if entry == nil {
		return nil
	}
	if entry.owner != owner {
		return fail("MCP_INVALID_OWNER", "Only the owning source can remove this MCP server")
	}
	if cleanupFailed && !closing {
		return fail("MCP_CLEANUP_INCOMPLETE", "MCP connection or probe cleanup failed; restart the Host before replacing this server")
	}
	entry.cancel()
	if err := m.cancelOAuthAttempt(ctx, name); err != nil {
		return err
	}
	m.withdraw(entry)
	m.mu.RLock()
	probes := make([]*serverProbe, 0, len(entry.probes))
	for probe := range entry.probes {
		probes = append(probes, probe)
	}
	m.mu.RUnlock()
	for _, probe := range probes {
		probe.cancel()
	}
	timer := time.NewTimer(CloseTimeout)
	defer timer.Stop()
	select {
	case <-entry.done:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		m.state(entry, "stopping", fail("MCP_CLEANUP_TIMEOUT", "MCP cleanup has not finished"))
		return fail("MCP_CLEANUP_TIMEOUT", "MCP cleanup has not finished")
	}
	for _, probe := range probes {
		select {
		case <-probe.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			m.state(entry, "stopping", fail("MCP_CLEANUP_TIMEOUT", "MCP probe cleanup has not finished"))
			return fail("MCP_CLEANUP_TIMEOUT", "MCP probe cleanup has not finished")
		}
	}
	// A probe can discover a cleanup failure while this removal is waiting
	// for it. Recheck after the drain before allowing a replacement to proceed.
	m.mu.RLock()
	cleanupFailed = entry.cleanupFailure != nil
	closing = m.closed
	m.mu.RUnlock()
	if cleanupFailed && !closing {
		return fail("MCP_CLEANUP_INCOMPLETE", "MCP connection or probe cleanup failed; restart the Host before replacing this server")
	}
	m.mu.RLock()
	connection := entry.connection
	m.mu.RUnlock()
	if connection != nil {
		if err := connection.Close(); err != nil {
			m.state(entry, "stopping", err)
			return err
		}
	}
	m.mu.Lock()
	if m.entries[name] == entry {
		delete(m.entries, name)
	}
	m.mu.Unlock()
	m.changed()
	if cleanupFailed {
		return fail("MCP_CLEANUP_INCOMPLETE", "MCP connection or probe cleanup failed before Host shutdown")
	}
	return nil
}

func (m *Manager) Register(ctx context.Context, name, owner string, config mcpconfig.Server) error {
	return m.register(ctx, name, owner, config)
}

func (m *Manager) register(ctx context.Context, name, owner string, config mcpconfig.Server) error {
	if !mcpconfig.ValidServerName(name) || owner == "" || len(owner) > 256 {
		return fail("MCP_INVALID_CONFIG", "Plugin MCP registration needs a server name and owner")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	resolved, missing, err := m.resolvePluginConfig(name, config)
	if err != nil {
		return err
	}
	config = resolved
	leave, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	m.mu.RLock()
	taken := m.entries[name] != nil
	m.mu.RUnlock()
	if taken {
		return fail("MCP_NAME_CONFLICT", "MCP server name is already configured or registered: "+name)
	}
	if config.CWD == "" && config.Transport == "stdio" {
		config.CWD = m.options.CWD
	}
	if err = m.upsert(ctx, name, owner, config, missing); err != nil {
		m.mu.Lock()
		if entry := m.entries[name]; entry != nil && entry.owner == owner {
			entry.registered = false
		}
		m.mu.Unlock()
		cleanup, cancel := context.WithTimeout(context.Background(), CloseTimeout)
		defer cancel()
		return errors.Join(err, m.remove(cleanup, name, owner))
	}
	return nil
}
func (m *Manager) Release(ctx context.Context, name, owner string) error {
	leave, err := m.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	m.mu.Lock()
	if entry := m.entries[name]; entry != nil && entry.owner == owner {
		entry.registered = false
	}
	m.mu.Unlock()
	m.changed()
	if err := m.remove(ctx, name, owner); err != nil {
		return err
	}
	m.mu.Lock()
	m.mu.Unlock()
	return nil
}
func (m *Manager) Restart(ctx context.Context, name string) (mcpconfig.Status, error) {
	leave, err := m.enter(ctx)
	if err != nil {
		return m.Status(), err
	}
	defer leave()
	m.mu.RLock()
	entry := m.entries[name]
	m.mu.RUnlock()
	if entry == nil || (entry.owner != "" && !entry.registered) {
		return m.Status(), fail("MCP_NOT_FOUND", "MCP server not configured")
	}
	config, owner := entry.config, entry.owner
	var missing []string
	if owner == "" {
		m.mu.RLock()
		settings := m.desired.Clone()
		m.mu.RUnlock()
		config, missing, err = settings.Resolve(name)
		if err != nil {
			return m.Status(), err
		}
		if config.CWD == "" && config.Transport == "stdio" {
			config.CWD = m.options.CWD
		}
	}
	if err = m.remove(ctx, name, owner); err == nil {
		err = m.upsert(ctx, name, owner, config, missing)
	}
	return m.Status(), err
}
func (m *Manager) Close() error {
	m.cancel()
	m.gate <- struct{}{}
	defer func() { <-m.gate }()
	m.mu.Lock()
	m.closed = true
	var entries []*serverEntry
	for _, entry := range m.entries {
		entries = append(entries, entry)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var combined error
	for _, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), CloseTimeout)
			defer cancel()
			err := m.remove(ctx, entry.name, entry.owner)
			if err != nil {
				mu.Lock()
				combined = errors.Join(combined, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if m.ownsCredentials {
		combined = errors.Join(combined, m.oauthStore.credentials.Close())
	}
	return combined
}

func (e *serverEntry) credentialKey() string {
	if e.config.OAuth != nil && e.config.OAuth.CredentialKey != "" {
		return e.config.OAuth.CredentialKey
	}
	return e.name
}

// Plugin declarations resolve explicit references through the same built-in
// credential Store as authorization. Node never owns an MCP secret table.
func (m *Manager) resolvePluginConfig(name string, config mcpconfig.Server) (mcpconfig.Server, []string, error) {
	settings := mcpconfig.Settings{Servers: map[string]mcpconfig.Server{name: config}, Credentials: map[string]string{}}
	resolved, missing, err := settings.Resolve(name)
	if err != nil || m.options.Credentials == nil {
		return resolved, missing, err
	}
	for _, ref := range missing {
		value, err := m.options.Credentials.Lookup(ref)
		if err != nil {
			return mcpconfig.Server{}, nil, err
		}
		if value != nil {
			settings.Credentials[ref] = value.Value
		}
	}
	return settings.Resolve(name)
}
