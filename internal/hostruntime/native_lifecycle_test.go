package hostruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/assembly"
	"go-bot/harness/permission"
	"go-bot/harness/session"
	"go-bot/internal/executor"
	"go-bot/internal/modeldata"
)

type lifecycleBuilder struct {
	nativeRuntimeBuilder
	closed func()
}

type lifecycleFailureBuilder struct {
	nativeRuntimeBuilder
	err error
}

func (b lifecycleFailureBuilder) BuildNative(ctx context.Context, identity assembly.NativeRuntimeIdentity, mode string) (assembly.Resources, error) {
	resource, err := b.nativeRuntimeBuilder.BuildNative(ctx, identity, mode)
	resource.Close = func() error { return b.err }
	return resource, err
}

func (b lifecycleBuilder) BuildNative(ctx context.Context, identity assembly.NativeRuntimeIdentity, mode string) (assembly.Resources, error) {
	resource, err := b.nativeRuntimeBuilder.BuildNative(ctx, identity, mode)
	resource.Close = func() error { b.closed(); return nil }
	return resource, err
}

func lifecycleFileHost(t *testing.T) (*NativeHost, sessionformat.FileStore) {
	t.Helper()
	claims := &sessionformat.ProcessClaims{}
	store := sessionformat.FileStore{Root: t.TempDir(), ProjectCWD: t.TempDir(), Claim: claims.Acquire}
	host := &NativeHost{UserID: "lifecycle-test"}
	host.Build = func(id string, _ bool) (*NativeRuntime, error) {
		factory := nativeRuntimeTestFactory(t, nil, nativeRuntimeModel{})
		factory.Storage = executor.NativeFileStorage{Store: store, SessionID: id}
		return &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: host.UserID, SessionID: id, Workspace: store.ProjectCWD}}, nil
	}
	host.ReadSession = store.ReadArtifact
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := host.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return host, store
}

func TestOwnedAgentDisposalCannotStopASameIDReplacement(t *testing.T) {
	host, _ := lifecycleFileHost(t)
	first, err := host.Activate(t.Context(), "owned", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.StopRuntime(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second, err := host.Activate(t.Context(), "owned", true)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("resume reused the disposed runtime")
	}
	for _, old := range []*NativeRuntime{first, {Identity: first.Identity}} {
		if err := host.StopRuntime(t.Context(), old); err != nil {
			t.Fatal(err)
		}
		if current, ok := host.Session("owned"); !ok || current != second {
			t.Fatal("a stale or unowned handle stopped the replacement")
		}
	}
	if err := host.StopRuntime(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := host.StopRuntime(t.Context(), second); err != nil {
		t.Fatal("disposal of an already released Agent is not idempotent", err)
	}
	if err := host.StopRuntime(t.Context(), nil); err == nil {
		t.Fatal("missing owner accepted")
	}
}

type ownedAgentBlockModel struct {
	started chan struct{}
	calls   atomic.Int32
}

func (m *ownedAgentBlockModel) Complete(ctx context.Context, _ []agent.Message, _ []agent.ToolSpec) (modeldata.AssistantMessage, error) {
	if m.calls.Add(1) == 1 {
		close(m.started)
	}
	<-ctx.Done()
	return modeldata.AssistantMessage{}, ctx.Err()
}

func TestOwnedAgentDisposalCancelsActiveTurnAndClearsReadyInbox(t *testing.T) {
	host, store := lifecycleFileHost(t)
	model := &ownedAgentBlockModel{started: make(chan struct{})}
	build := host.Build
	host.Build = func(id string, resume bool) (*NativeRuntime, error) {
		runtime, err := build(id, resume)
		runtime.Factory.Model = model
		return runtime, err
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	runtime, err := host.Activate(ctx, "owned-cancel", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SubmitText(ctx, "active"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := runtime.SubmitText(ctx, "queued"); err != nil {
		t.Fatal(err)
	}
	if err := host.StopRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	artifact, err := store.ReadArtifact(ctx, "owned-cancel")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := sessionformat.FoldInbox(artifact.Events)
	if err != nil || len(inbox.NextStep) != 0 || len(inbox.NextTurn) != 0 || model.calls.Load() != 1 {
		t.Fatal("owner teardown drained queued work instead of cancelling it", inbox, model.calls.Load(), err)
	}
	var reason struct {
		Reason struct {
			Kind   string
			Reason struct{ Kind string }
		}
	}
	for _, event := range artifact.Events {
		if event.Type == "turn/end" {
			if err := json.Unmarshal(event.Data, &reason); err != nil {
				t.Fatal(err)
			}
		}
	}
	if reason.Reason.Kind != "aborted" || reason.Reason.Reason.Kind != "disposed" {
		t.Fatal("owner teardown lost the disposed cancellation cause", reason)
	}
}

func TestAwaitSessionWaitsForPreparedGoAgentAndPropagatesFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "failed"}[fail], func(t *testing.T) {
			host, _ := lifecycleFileHost(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			activation := make(chan error, 1)
			rejected := errors.New("setup rejected")
			go func() {
				_, err := host.ActivateWithSetup(ctx, "awaited", false, func(context.Context, *assembly.NativeLive) error {
					close(entered)
					<-release
					if fail {
						return rejected
					}
					return nil
				})
				activation <- err
			}()
			<-entered
			waited := make(chan error, 1)
			go func() { _, err := host.AwaitSession(ctx, "awaited"); waited <- err }()
			select {
			case err := <-waited:
				t.Fatal("unpublished Agent escaped the creation barrier", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-activation; fail != errors.Is(err, rejected) {
				t.Fatal("unexpected setup outcome", err)
			}
			if err := <-waited; fail != errors.Is(err, rejected) {
				t.Fatal("waiting caller lost setup outcome", err)
			}
		})
	}
}

func TestPreparedGoSessionIsReadableBeforeAgentPublicationAndSetupRollbackRetainsFacts(t *testing.T) {
	host, store := lifecycleFileHost(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var bound, closed atomic.Int32
	build := host.Build
	host.Build = func(id string, resume bool) (*NativeRuntime, error) {
		runtime, err := build(id, resume)
		runtime.Factory.BeforePublish = func(context.Context, *assembly.NativeLive) error { bound.Add(1); return nil }
		runtime.Factory.Builder = lifecycleBuilder{nativeRuntimeBuilder{store.ProjectCWD}, func() { closed.Add(1) }}
		return runtime, err
	}
	failed := errors.New("setup rejected")
	_, err := host.ActivateWithSetup(ctx, "prepared", false, func(ctx context.Context, live *assembly.NativeLive) error {
		if _, active := host.Session("prepared"); active {
			return errors.New("Agent published before setup")
		}
		activity, err := host.Activity()
		if err != nil || activity.Idle {
			return errors.New("preparation reported idle")
		}
		artifact, err := host.SessionArtifact(ctx, "prepared")
		if err != nil || artifact.Header != live.Session.Header() {
			return errors.New("prepared history did not use the actual Go Session")
		}
		if err := host.WithSessionHistory(ctx, "other", func() error { return nil }); err != nil {
			return err
		}
		if _, err := host.Activate(ctx, "nested", false); err != nil {
			return err
		}
		if err := permission.Save(ctx, live.Session, permission.ReadOnly, false, true); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatal("setup did not return its original failure", err)
	}
	if _, found := host.Session("prepared"); found || bound.Load() != 2 || closed.Load() != 1 {
		t.Fatal("failed setup published/bound the Agent or retained resources")
	}
	artifact, err := store.ReadArtifact(ctx, "prepared")
	if err != nil || permission.Read(artifact.Events).Mode != permission.ReadOnly {
		t.Fatal("rollback deleted committed Session facts", err)
	}
	resumed, err := host.Activate(ctx, "prepared", true)
	if err != nil {
		t.Fatal("failed setup left the file claim owned", err)
	}
	state, err := resumed.GetPermission()
	if err != nil || state.Mode != permission.ReadOnly {
		t.Fatal("resume did not restore the actual journal state", state, err)
	}
	if bound.Load() != 3 {
		t.Fatal("resume did not publish once")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.historyLocks) != 0 || len(host.operations) != 0 {
		t.Fatal("completed preparation retained a gate or operation")
	}
}

func TestCloseCancelsAndDrainsPreparedGoAgentWithoutLatePublication(t *testing.T) {
	host, _ := lifecycleFileHost(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var bound atomic.Int32
	build := host.Build
	host.Build = func(id string, resume bool) (*NativeRuntime, error) {
		runtime, err := build(id, resume)
		runtime.Factory.BeforePublish = func(context.Context, *assembly.NativeLive) error { bound.Add(1); return nil }
		return runtime, err
	}
	finished := make(chan error, 1)
	go func() {
		_, err := host.ActivateWithSetup(t.Context(), "pending", false, func(context.Context, *assembly.NativeLive) error {
			close(entered)
			<-release // Deliberately ignores cancellation; cleanup must wait.
			return nil
		})
		finished <- err
	}()
	<-entered
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	if _, err := host.Activate(t.Context(), "pending", false); !errors.Is(err, ErrHostSessionActive) {
		t.Fatal("duplicate prepared lifetime was admitted", err)
	}
	if err := host.StopSession(t.Context(), "pending"); !errors.Is(err, ErrHostSessionBusy) {
		t.Fatal("unpublished creation exposed an ordinary stop capability", err)
	}
	host.SaveArchive = func(context.Context, string) error { t.Error("archived a pending creation"); return nil }
	if err := host.ArchiveSession(t.Context(), "pending"); !errors.Is(err, ErrHostSessionBusy) {
		t.Fatal("archive did not guard pending creation", err)
	}
	short, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := host.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("close did not honor its cleanup budget", err)
	}
	host.mu.Lock()
	pending := host.operations["pending"] != nil
	host.mu.Unlock()
	if !pending {
		t.Fatal("timed-out close abandoned ownership")
	}
	close(release)
	released = true
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled setup succeeded", err)
	}
	if bound.Load() != 1 {
		t.Fatal("cancelled setup published extension bindings")
	}
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := host.Close(ctx); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.sessions) != 0 || len(host.operations) != 0 || len(host.historyLocks) != 0 {
		t.Fatal("closed Host retained a prepared lifetime")
	}
}

func TestStoppingAgentCanReenterHostCreationFromItsResourceDisposer(t *testing.T) {
	host, store := lifecycleFileHost(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	build := host.Build
	var callbackErr error
	host.Build = func(id string, resume bool) (*NativeRuntime, error) {
		runtime, err := build(id, resume)
		if id == "first" {
			runtime.Factory.Builder = lifecycleBuilder{nativeRuntimeBuilder{store.ProjectCWD}, func() { _, callbackErr = host.Activate(ctx, "from-disposer", false) }}
		}
		return runtime, err
	}
	if _, err := host.Activate(ctx, "first", false); err != nil {
		t.Fatal(err)
	}
	if err := host.StopSession(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if callbackErr != nil {
		t.Fatal("disposal reentry failed", callbackErr)
	}
	if _, found := host.Session("from-disposer"); !found {
		t.Fatal("disposer's actual Agent was not published")
	}
}

func TestSchedulePreparationWaitsForTheAlreadyAdmittedGoCreation(t *testing.T) {
	host, _ := lifecycleFileHost(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	activation := make(chan error, 1)
	go func() {
		_, err := host.ActivateWithSetup(ctx, "scheduled", false, func(context.Context, *assembly.NativeLive) error {
			close(entered)
			<-release
			return nil
		})
		activation <- err
	}()
	<-entered
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	prepared := make(chan error, 1)
	go func() { prepared <- host.PrepareSchedule(ctx, "scheduled") }()
	select {
	case err := <-prepared:
		t.Fatal("Schedule did not await the admitted creation", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	released = true
	if err := <-activation; err != nil {
		t.Fatal(err)
	}
	if err := <-prepared; err != nil {
		t.Fatal("Schedule lost its original destination", err)
	}
}

func TestFailedPreparedAgentCleanupRetainsOwnershipAndRejectsReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	setupFailure, cleanupFailure := errors.New("setup failed"), errors.New("cleanup was not confirmed")
	host := &NativeHost{UserID: "cleanup-owner"}
	host.Build = func(id string, _ bool) (*NativeRuntime, error) {
		factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
		factory.Builder = lifecycleFailureBuilder{nativeRuntimeBuilder{t.TempDir()}, cleanupFailure}
		return &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{UserID: host.UserID, SessionID: id, Workspace: t.TempDir()}}, nil
	}
	_, err := host.ActivateWithSetup(ctx, "retained", false, func(context.Context, *assembly.NativeLive) error { return setupFailure })
	if !errors.Is(err, setupFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatal("creation concealed a failed rollback", err)
	}
	if _, retained := host.Session("retained"); !retained {
		t.Fatal("failed cleanup lost its owner")
	}
	activity, err := host.Activity()
	if err != nil || activity.Idle {
		t.Fatal("failed cleanup reported idle", err)
	}
	if _, err := host.Activate(ctx, "retained", false); !errors.Is(err, ErrHostSessionActive) {
		t.Fatal("replacement overlapped unconfirmed cleanup", err)
	}
	if err := host.Close(ctx); !errors.Is(err, cleanupFailure) {
		t.Fatal("close reported unconfirmed cleanup as success", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.sessions["retained"] == nil {
		t.Fatal("failed close abandoned ownership")
	}
}
