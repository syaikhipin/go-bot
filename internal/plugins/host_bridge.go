package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go-bot/internal/credentials"
	"go-bot/plugin"
)

// hostBridge is the Go endpoint of the new ctx.host contract. It owns no
// Agent or Session: those remain in the existing Harness runtime.
type hostBridge struct {
	config        RuntimeConfig
	peer          *peer
	command       *exec.Cmd
	directory     string
	done          chan struct{}
	closed        chan struct{}
	closeOnce     sync.Once
	applyMu       sync.Mutex
	mu            sync.RWMutex
	registrations map[string]*hostContribution
	sequence      uint64
	invocations   sync.Map
	notifications hostNotifications
	unsubscribers []func()
}

type hostPluginInstance = plugin.InstanceConfig

type hostContribution struct {
	ID         string          `json:"id"`
	Owner      string          `json:"owner"`
	Kind       string          `json:"kind"`
	CallbackID string          `json:"callbackId"`
	Descriptor json.RawMessage `json:"descriptor"`
	Callbacks  []string        `json:"callbacks"`
	order      uint64
	ctx        context.Context
	cancel     context.CancelFunc
	tool       *hostTool
	resourceMu sync.Mutex
	cleanup    func(context.Context) error
}

var bridgeInstanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

func startHostBridge(ctx context.Context, c RuntimeConfig) (*hostBridge, error) {
	if !filepath.IsAbs(c.Main) || !filepath.IsAbs(c.ProfileDir) {
		return nil, errors.New("Host Bridge needs absolute entry and package paths")
	}
	if c.Node == "" {
		c.Node = "node"
	}
	if err := os.MkdirAll(c.ProfileDir, 0700); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(c.SocketParent, "go-bot-bridge-")
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "rpc.sock"), Net: "unix"})
	if err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	defer listener.Close()
	if err = os.Chmod(filepath.Join(directory, "rpc.sock"), 0600); err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	r := &hostBridge{config: c, directory: directory, done: make(chan struct{}), closed: make(chan struct{}),
		registrations: map[string]*hostContribution{}}
	r.command = exec.Command(c.Node, c.Main, filepath.Join(directory, "rpc.sock"))
	r.command.Dir = c.ProfileDir
	r.command.Env = runtimeEnvironment()
	for name, value := range map[string]string{"HOME": c.Home, "DSH_HOME": c.DSHHome} {
		if value != "" {
			r.command.Env = append(r.command.Env, name+"="+value)
		}
	}
	r.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.command.Stdout, r.command.Stderr = io.Discard, io.Discard
	if err = r.command.Start(); err != nil {
		os.RemoveAll(directory)
		return nil, err
	}
	go func() { _ = r.command.Wait(); close(r.done) }()
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-r.done:
		err = errors.New("Host Bridge exited before connecting")
	case <-startup.Done():
		err = startup.Err()
	}
	if conn == nil {
		listener.Close()
		_ = syscall.Kill(-r.command.Process.Pid, syscall.SIGKILL)
		<-r.done
		select {
		case pending := <-accepted:
			if pending != nil {
				pending.Close()
			}
		default:
		}
		os.RemoveAll(directory)
		return nil, fmt.Errorf("start Host Bridge: %w", err)
	}
	r.peer = newPeer(conn, r.handle)
	var status []any
	if err = r.peer.call(startup, "runtime.inspect", nil, &status); err != nil {
		r.Close()
		return nil, err
	}
	r.startNotifications()
	go func() {
		select {
		case <-ctx.Done():
		case <-r.peer.closed:
		case <-r.done:
		case <-r.closed:
			return
		}
		_ = r.Close()
	}()
	return r, nil
}

func (r *hostBridge) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "bridge.initialize":
		var input struct {
			Protocol int    `json:"protocol"`
			API      string `json:"api"`
		}
		if json.Unmarshal(raw, &input) != nil || input.Protocol != 1 || input.API != "1" {
			return nil, failure("INCOMPATIBLE_PLUGIN", "Unsupported Host Bridge protocol")
		}
		return map[string]any{"protocol": 1, "api": "1", "directory": r.config.ProfileDir,
			"maxFrameBytes": maxFrame,
			"capabilities":  []string{"info", "tools", "skills", "prompts", "credentials", "authorization", "mcp", "approvals", "questions"}}, nil
	case "contribution.register":
		var row hostContribution
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		if err := r.register(ctx, &row); err != nil {
			return nil, err
		}
		return map[string]string{"id": row.ID}, nil
	case "contribution.remove":
		var input struct {
			Owner string `json:"owner"`
			ID    string `json:"registrationId"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		return nil, r.remove(input.ID, input.Owner)
	case "host.invoke":
		return r.invokeHost(ctx, raw)
	case "runtime.log":
		var input struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		if r.config.Log != nil {
			if r.config.Credentials != nil {
				input.Message = r.config.Credentials.Redact(input.Message)
			}
			r.config.Log(input.Level, input.Message)
		}
		return nil, nil
	default:
		return nil, failure("NOT_FOUND", "Unknown Host Bridge method")
	}
}

func (r *hostBridge) register(ctx context.Context, row *hostContribution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !bridgeInstanceName.MatchString(row.Owner) || !identifier.MatchString(row.CallbackID) || !json.Valid(row.Descriptor) {
		return failure("INVALID_ARGUMENT", "Invalid plugin contribution")
	}
	var err error
	switch row.Kind {
	case "tool":
		row.tool, err = r.newTool(row)
	case "prompt":
		err = validateHostPrompt(row)
	case "skill", "skill-directory", "skill-provider":
		err = validateHostSkill(row)
	case "authorization":
		err = validateHostAuthorization(row)
	case "mcp":
		err = validateHostMCP(row)
	default:
		return failure("UNAVAILABLE", "This contribution kind has not been connected to Host Bridge")
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	for _, current := range r.registrations {
		if row.tool != nil && current.tool != nil && row.tool.Spec().Name == current.tool.Spec().Name {
			r.mu.Unlock()
			return failure("CONFLICT", "Plugin tool name is already registered")
		}
	}
	row.ID = uuid.NewString()
	r.sequence++
	row.order = r.sequence
	row.ctx, row.cancel = context.WithCancel(context.Background())
	r.registrations[row.ID] = row
	r.mu.Unlock()
	if row.Kind == "authorization" || row.Kind == "mcp" {
		var cleanup func(context.Context) error
		var err error
		if row.Kind == "authorization" {
			cleanup, err = r.registerAuthorizationResource(row)
		} else {
			cleanup, err = r.registerMCPResource(ctx, row)
		}
		if err != nil {
			_ = r.remove(row.ID, row.Owner)
			return err
		}
		row.resourceMu.Lock()
		row.cleanup = cleanup
		closed := row.ctx.Err() != nil
		row.resourceMu.Unlock()
		if closed {
			_ = cleanup(context.WithoutCancel(ctx))
			return failure("CANCELLED", "The contribution was removed while registering")
		}
	}
	if err := ctx.Err(); err != nil {
		_ = r.remove(row.ID, row.Owner)
		return err
	}
	r.contributionChanged(row)
	return nil
}

func (r *hostBridge) remove(id, owner string) error {
	r.mu.Lock()
	row := r.registrations[id]
	if row == nil {
		r.mu.Unlock()
		return nil
	}
	if row.Owner != owner {
		r.mu.Unlock()
		return failure("INVALID_ARGUMENT", "The contribution belongs to another plugin instance")
	}
	row.cancel()
	delete(r.registrations, id)
	r.mu.Unlock()
	r.contributionChanged(row)
	row.resourceMu.Lock()
	cleanup := row.cleanup
	row.resourceMu.Unlock()
	if cleanup != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return cleanup(ctx)
	}
	return nil
}

func (r *hostBridge) rows(kind string) []*hostContribution {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var rows []*hostContribution
	for _, row := range r.registrations {
		if row.Kind == kind {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].order < rows[j].order })
	return rows
}

func (r *hostBridge) callback(ctx context.Context, row *hostContribution, method string, args any, call any, out any) error {
	found := false
	for _, name := range row.Callbacks {
		if name == method {
			found = true
			break
		}
	}
	if !found {
		return failure("NOT_FOUND", "Plugin callback is unavailable")
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(row.ctx, cancel)
	defer stop()
	if row.ctx.Err() != nil {
		return failure("UNAVAILABLE", "Plugin registration has been disposed")
	}
	err := r.peer.callDrained(callCtx, "callback.invoke", map[string]any{"owner": row.Owner, "callbackId": row.CallbackID, "method": method, "args": args, "context": call}, out)
	if fatalDrain(err) {
		r.peer.close()
	}
	return err
}

func (r *hostBridge) Apply(ctx context.Context, instances []hostPluginInstance) ([]map[string]any, error) {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	ids := map[string]bool{}
	for _, instance := range instances {
		if !bridgeInstanceName.MatchString(instance.ID) {
			return nil, failure("INVALID_ARGUMENT", "Invalid plugin instance id")
		}
		if ids[instance.ID] {
			return nil, failure("CONFLICT", "Duplicate plugin instance id")
		}
		ids[instance.ID] = true
	}
	var response struct {
		Instances []map[string]any `json:"instances"`
	}
	err := r.peer.call(ctx, "runtime.apply", map[string]any{"instances": instances}, &response)
	return response.Instances, err
}

func (r *hostBridge) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		for _, unsubscribe := range r.unsubscribers {
			unsubscribe()
		}
		if r.peer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = r.peer.call(ctx, "runtime.shutdown", nil, nil)
			cancel()
			r.peer.close()
		}
		if r.command != nil {
			_ = syscall.Kill(-r.command.Process.Pid, syscall.SIGTERM)
			select {
			case <-r.done:
			case <-time.After(3 * time.Second):
				_ = syscall.Kill(-r.command.Process.Pid, syscall.SIGKILL)
				<-r.done
			}
		}
		r.mu.Lock()
		remaining := make([]*hostContribution, 0, len(r.registrations))
		for _, row := range r.registrations {
			row.cancel()
			remaining = append(remaining, row)
		}
		r.registrations = map[string]*hostContribution{}
		r.mu.Unlock()
		for _, row := range remaining {
			row.resourceMu.Lock()
			cleanup := row.cleanup
			row.resourceMu.Unlock()
			if cleanup != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = cleanup(ctx)
				cancel()
			}
		}
		_ = os.RemoveAll(r.directory)
	})
	return nil
}

func (r *hostBridge) invokeHost(ctx context.Context, raw json.RawMessage) (any, error) {
	var input struct {
		Owner      string          `json:"owner"`
		Service    string          `json:"service"`
		Method     string          `json:"method"`
		Input      json.RawMessage `json:"input"`
		Mutation   string          `json:"mutation"`
		Discovery  bool            `json:"discovery"`
		Invocation string          `json:"invocation"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if !bridgeInstanceName.MatchString(input.Owner) {
		return nil, failure("INVALID_ARGUMENT", "Invalid plugin source")
	}
	if input.Service == "credentials" {
		return r.credential(ctx, input.Owner, input.Method, input.Input, input.Mutation)
	}
	if input.Service == "proactive" && input.Method == "changed" {
		if r.config.ProactiveChanged == nil {
			return nil, failure("UNAVAILABLE", "Proactive information signaling is not configured")
		}
		if err := r.config.ProactiveChanged(); err != nil {
			return nil, failure("UNAVAILABLE", "The information change could not be recorded")
		}
		return nil, nil
	}
	if input.Service == "approvals" && input.Method == "request" {
		return r.approvalCall(ctx, input.Owner, input.Invocation, input.Input)
	}
	if input.Service == "questions" && input.Method == "ask" {
		return r.questionCall(ctx, input.Owner, input.Invocation, input.Input)
	}
	if input.Service == "authorization" {
		return r.authorizationRequest(ctx, input.Owner, input.Method, input.Input, input.Mutation)
	}
	if input.Service == "mcp" {
		return r.mcpRequest(ctx, input.Owner, input.Method, input.Input)
	}
	if input.Service == "skills" {
		if input.Method == "invalidate" {
			r.notify("host/skills-changed", "skill:"+input.Owner, map[string]string{"instanceId": input.Owner, "source": "invalidate"})
		}
		return r.skillRequest(ctx, input.Method, input.Input, input.Discovery)
	}
	if input.Service == "info" && input.Method == "get" {
		return map[string]string{"userId": r.config.UserID, "hostId": r.config.HostID, "dataRoot": r.config.Workspace, "apiVersion": "1", "instanceId": input.Owner}, nil
	}
	return nil, failure("UNAVAILABLE", "The requested Host service has not been connected")
}

func credentialAddress(owner, key string) (string, error) {
	if !bridgeInstanceName.MatchString(owner) || !bridgeInstanceName.MatchString(key) {
		return "", failure("INVALID_ARGUMENT", "Invalid plugin credential key")
	}
	return owner + "/" + key, nil
}

func (r *hostBridge) credential(ctx context.Context, owner, method string, raw json.RawMessage, mutation string) (any, error) {
	if r.config.Credentials == nil {
		return nil, failure("UNAVAILABLE", "Go credential service is not configured")
	}
	var input struct {
		Key      string              `json:"key"`
		Ref      string              `json:"ref"`
		Record   *credentials.Record `json:"record"`
		Callback string              `json:"callback"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	if method == "resolve" {
		value, err := r.config.Credentials.Resolve(input.Ref)
		if value == nil {
			return nil, err
		}
		return value.Value, err
	}
	if method == "list" {
		rows, err := r.config.Credentials.ListRecords()
		if err != nil {
			return nil, err
		}
		result := []map[string]string{}
		for _, row := range rows {
			if key, ok := strings.CutPrefix(row.Key, owner+"/"); ok {
				result = append(result, map[string]string{"key": key, "kind": row.Kind})
			}
		}
		return result, nil
	}
	key, err := credentialAddress(owner, input.Key)
	if err != nil {
		return nil, err
	}
	if mutation != "" && (method == "put" || method == "update" || method == "remove") {
		return nil, failure("CONFLICT", "A credential mutation cannot recursively write the credential store")
	}
	switch method {
	case "get":
		return r.config.Credentials.ReadRecord(key)
	case "describe":
		return r.config.Credentials.DescribeRecord(key)
	case "remove":
		return nil, r.config.Credentials.DeleteRecord(ctx, key)
	case "put":
		if input.Record == nil {
			return nil, failure("INVALID_ARGUMENT", "A credential record is required")
		}
		_, err := r.config.Credentials.ModifyRecord(ctx, key, func(*credentials.Record) (*credentials.Record, error) { return input.Record, nil })
		return nil, err
	case "update":
		if !identifier.MatchString(input.Callback) {
			return nil, failure("INVALID_ARGUMENT", "A credential mutation callback is required")
		}
		return r.config.Credentials.ModifyRecord(ctx, key, func(current *credentials.Record) (*credentials.Record, error) {
			var next *credentials.Record
			err := r.peer.callDrained(ctx, "callback.invoke", map[string]any{"owner": owner, "callbackId": input.Callback, "method": "mutate", "args": current, "context": map[string]string{"mutation": uuid.NewString()}}, &next)
			return next, err
		})
	default:
		return nil, failure("NOT_FOUND", "Unknown credential method")
	}
}

func (r *hostBridge) Alive() bool {
	select {
	case <-r.closed:
		return false
	case <-r.done:
		return false
	case <-r.peer.closed:
		return false
	default:
		return true
	}
}
