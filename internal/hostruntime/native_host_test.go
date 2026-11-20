package hostruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/assembly"
	"go-bot/harness/interaction"
)

func TestHostStreamAudienceIncludesFutureSessionsAndMultipleClients(t *testing.T) {
	host := &NativeHost{}
	if host.hasChatStream("new-session") {
		t.Fatal("offline Host has an audience")
	}
	first, second := host.AttachHostStream(), host.AttachHostStream()
	if !host.hasChatStream("new-session") {
		t.Fatal("Host client did not cover a future Session")
	}
	first()
	first() // A teardown must never decrement twice.
	if !host.hasChatStream("another-session") {
		t.Fatal("one remaining client was ignored")
	}
	second()
	if host.hasChatStream("new-session") {
		t.Fatal("closed clients remained online")
	}
}

func TestNativeHostOwnsMultipleIndependentSessions(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*nativeRuntimeStore{
		"first":  {},
		"second": {},
	}
	var buildResumes []bool
	host := &NativeHost{UserID: "alice", Build: func(id string, resume bool) (*NativeRuntime, error) {
		buildResumes = append(buildResumes, resume)
		return &NativeRuntime{
			Factory: nativeRuntimeTestFactory(t, stores[id], nativeRuntimeModel{}),
			Identity: assembly.NativeRuntimeIdentity{
				UserID: "alice", SessionID: id, Workspace: "/workspace",
			},
		}, nil
	}}
	first, err := host.Activate(ctx, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := host.Activate(ctx, "second", false)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	if first == second {
		t.Fatal("two Sessions shared one Agent runtime")
	}
	if _, err := host.Activate(ctx, "first", false); !errors.Is(err, ErrHostSessionActive) {
		t.Fatalf("duplicate activation = %v", err)
	}
	if _, err := first.SubmitText(ctx, "first task"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SubmitText(ctx, "second task"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, first, 1)
	waitNativeTurn(t, second, 1)
	if err := host.StopSession(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	if _, found := host.Session("first"); found {
		t.Fatal("stopped Session remains routable")
	}
	if got, found := host.Session("second"); !found || got != second {
		t.Fatal("stopping first Session removed second Session")
	}
	if _, err := second.SubmitText(ctx, "still running"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, second, 2)
	resumed, err := host.Activate(ctx, "first", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(buildResumes) != 3 || buildResumes[0] || buildResumes[1] || !buildResumes[2] {
		t.Fatalf("Host builder did not receive create/create/resume sequence: %v", buildResumes)
	}
	if _, err := resumed.SubmitText(ctx, "continued"); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, resumed, 2)
	for id, store := range stores {
		if store.header.ID != id {
			t.Fatalf("%s journal belongs to %q", id, store.header.ID)
		}
		if len(store.events) == 0 {
			t.Fatalf("%s journal is empty", id)
		}
	}
	activity, err := host.Activity()
	if err != nil || activity.Since.IsZero() {
		t.Fatalf("Host activity = %+v, %v", activity, err)
	}
}

func TestNativeHostActivityIncludesSessionActivation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	host := &NativeHost{UserID: "alice", StartedAt: time.Now().Add(-25 * time.Hour), Build: func(string, bool) (*NativeRuntime, error) {
		close(entered)
		<-release
		return nil, errors.New("builder unavailable")
	}}
	result := make(chan error, 1)
	go func() {
		_, err := host.Activate(context.Background(), "new-session", false)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Session activation did not enter the builder")
	}
	activity, err := host.Activity()
	if err != nil || activity.Idle {
		close(release)
		t.Fatalf("Host reported idle during Session activation: %+v, %v", activity, err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("builder failure was lost")
	}
	activity, err = host.Activity()
	if err != nil || !activity.Idle {
		t.Fatalf("failed activation left Host busy: %+v, %v", activity, err)
	}
}

func TestNativeHostActivityIncludesChildJobAcrossSessions(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	model := &nativeJobModel{notified: make(chan struct{})}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model)
	base := factory.Builder.(nativeRuntimeBuilder)
	childJobs := &nativeTreeJobBuilder{nativeRuntimeBuilder: base}
	factory.Storage = nativeTreeNode{tree: tree, id: "first"}
	factory.Builder = childJobs
	secondStore := &nativeRuntimeStore{}
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		chosen := factory
		if id == "second" {
			chosen = nativeRuntimeTestFactory(t, secondStore, nativeRuntimeModel{})
		}
		return &NativeRuntime{Factory: chosen, Identity: assembly.NativeRuntimeIdentity{
			UserID: "alice", SessionID: id, Workspace: "/workspace",
		}}, nil
	}}
	first, err := host.Activate(ctx, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Activate(ctx, "second", false); err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	childID, err := first.SpawnChild(ctx, "first", "Research", "Check the facts", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: childID}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "turn/end") })
	registry := childJobs.Registry(childID)
	if registry == nil {
		t.Fatal("child has no Job Registry")
	}
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	job, err := registry.Start("shell", "child task", func(context.Context, func([]byte)) (int, error) {
		<-release
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkVisible(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := host.StopSession(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if activity, err := host.Activity(); err != nil || activity.Idle {
		t.Fatalf("Host ignored child Job after other Session stopped: %+v, %v", activity, err)
	}
	close(release)
	nativeWaitFor(t, func() bool {
		activity, err := host.Activity()
		return err == nil && activity.Idle
	})
}

func TestNativeHostCloseDrainsRootAndChildJobs(t *testing.T) {
	ctx := context.Background()
	tree := &nativeTreeStore{}
	factory := nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{})
	builder := &nativeTreeJobBuilder{nativeRuntimeBuilder: factory.Builder.(nativeRuntimeBuilder)}
	factory.Storage, factory.Builder = nativeTreeNode{tree: tree, id: "root"}, builder
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		return &NativeRuntime{Factory: factory, Identity: assembly.NativeRuntimeIdentity{
			UserID: "alice", SessionID: id, Workspace: "/workspace",
		}}, nil
	}}
	root, err := host.Activate(ctx, "root", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	childID, err := root.SpawnChild(ctx, "root", "Research", "Check the facts", false)
	if err != nil {
		t.Fatal(err)
	}
	childStore := nativeTreeNode{tree: tree, id: childID}
	nativeWaitFor(t, func() bool { return nativeTreeHasEvent(childStore, "turn/end") })
	cleaned := make([]chan struct{}, 0, 2)
	for _, id := range []string{"root", childID} {
		registry := builder.Registry(id)
		if registry == nil {
			t.Fatalf("missing Job Registry for %s", id)
		}
		entered, done := make(chan struct{}), make(chan struct{})
		job, err := registry.Start("shell", "wait for Host close", func(ctx context.Context, _ func([]byte)) (int, error) {
			close(entered)
			<-ctx.Done()
			close(done)
			return 0, ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.MarkVisible(job.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("Job %s did not begin", id)
		}
		cleaned = append(cleaned, done)
	}
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := host.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	for index, done := range cleaned {
		select {
		case <-done:
		default:
			t.Fatalf("Host returned before Job %d producer cleanup", index)
		}
	}
}

func TestNativeHostCloseRetriesSessionAfterDeadline(t *testing.T) {
	ctx := context.Background()
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		return &NativeRuntime{
			Factory:  nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, nativeRuntimeModel{}),
			Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: id, Workspace: "/workspace"},
		}, nil
	}}
	runtime, err := host.Activate(ctx, "root", false)
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(blocked)
		}
		_ = host.Close(ctx)
	}()
	runtime.mu.Lock()
	runtime.children = newNativeChildManager(runtime.Factory, runtime.Identity, runtime.live, runtime.kick)
	runtime.children.mu.Lock()
	runtime.children.children["blocked"] = &nativeChildProcess{done: blocked, cancel: func() {}}
	runtime.children.mu.Unlock()
	runtime.mu.Unlock()
	firstCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := host.Close(firstCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Host close = %v, want deadline", err)
	}
	if host.sessions["root"] == nil {
		t.Fatal("Host lost the unfinished Session after a timed-out close")
	}
	close(blocked)
	released = true
	if err := host.Close(ctx); err != nil {
		t.Fatalf("retrying Host close = %v", err)
	}
	if len(host.sessions) != 0 {
		t.Fatalf("Host retained %d fully stopped Sessions", len(host.sessions))
	}
}

func TestNativeHostActivityIncludesPendingQuestionAcrossSessions(t *testing.T) {
	ctx := context.Background()
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		var model agent.Model = nativeRuntimeModel{}
		if id == "first" {
			model = &nativeQuestionModel{}
		}
		return &NativeRuntime{
			Factory: nativeRuntimeTestFactory(t, &nativeRuntimeStore{}, model),
			Identity: assembly.NativeRuntimeIdentity{
				UserID: "alice", SessionID: id, Workspace: "/workspace",
			},
		}, nil
	}}
	first, err := host.Activate(ctx, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Activate(ctx, "second", false); err != nil {
		t.Fatal(err)
	}
	defer host.Close(ctx)
	if _, err := first.SubmitText(ctx, "Ask before continuing"); err != nil {
		t.Fatal(err)
	}
	nativeWaitFor(t, func() bool {
		pending, err := first.PendingQuestions()
		return err == nil && len(pending) == 1 && pending[0].CallID == "question-call"
	})
	if activity, err := host.Activity(); err != nil || activity.Idle {
		t.Fatalf("Host ignored a pending question in one Session: %+v, %v", activity, err)
	}
	if err := first.AnswerQuestion(ctx, "question-call", interaction.Answer{Answers: []interaction.AnswerItem{{ID: "choice", Selected: []string{"B"}}}}); err != nil {
		t.Fatal(err)
	}
	waitNativeTurn(t, first, 1)
	nativeWaitFor(t, func() bool {
		activity, err := host.Activity()
		return err == nil && activity.Idle
	})
}

func TestNativeHostKeepsStoppingSessionVisibleUntilCleanupCompletes(t *testing.T) {
	ctx := context.Background()
	model := &nativeGateModel{started: make(chan struct{}), release: make(chan struct{})}
	store := &nativeRuntimeStore{}
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		return &NativeRuntime{
			Factory: nativeRuntimeTestFactory(t, store, model),
			Identity: assembly.NativeRuntimeIdentity{
				UserID: "alice", SessionID: id, Workspace: "/workspace",
			},
		}, nil
	}}
	runtime, err := host.Activate(ctx, "stopping", false)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	if _, err := runtime.SubmitText(ctx, "work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model turn did not begin")
	}
	stopCtx, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if err := host.StopSession(stopCtx, "stopping"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out Session stop = %v", err)
	}
	if got, found := host.Session("stopping"); !found || got != runtime {
		t.Fatal("unfinished Session disappeared from Host registry")
	}
	if activity, err := host.Activity(); err != nil || activity.Idle {
		t.Fatalf("Host considered a stopping Session idle: %+v, %v", activity, err)
	}
	finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
	defer finishCancel()
	if err := host.StopSession(finishCtx, "stopping"); err != nil {
		t.Fatal(err)
	}
	if _, found := host.Session("stopping"); found {
		t.Fatal("fully stopped Session remains in Host registry")
	}
}

func TestNativeHostRejectsForeignOrMissingSessions(t *testing.T) {
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		return &NativeRuntime{Identity: assembly.NativeRuntimeIdentity{UserID: "bob", SessionID: id}}, nil
	}}
	if _, err := host.Activate(context.Background(), "first", false); err == nil {
		t.Fatal("Host accepted a runtime belonging to another user")
	}
	if _, err := host.Activate(context.Background(), "../escape", false); err == nil {
		t.Fatal("Host accepted a path-like Session ID")
	}
	if err := host.StopSession(context.Background(), "missing"); !errors.Is(err, ErrHostSessionAbsent) {
		t.Fatalf("stop missing Session = %v", err)
	}
	if err := host.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Activate(context.Background(), "first", false); !errors.Is(err, ErrHostClosed) {
		t.Fatalf("activation after Host close = %v", err)
	}
	if _, err := host.Activity(); !errors.Is(err, ErrHostClosed) {
		t.Fatalf("activity after Host close = %v", err)
	}
}

func TestNativeHostMissingResumeDoesNotCreateSession(t *testing.T) {
	ctx := context.Background()
	store := &nativeRuntimeStore{}
	host := &NativeHost{UserID: "alice", Build: func(id string, _ bool) (*NativeRuntime, error) {
		return &NativeRuntime{
			Factory:  nativeRuntimeTestFactory(t, store, nativeRuntimeModel{}),
			Identity: assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: id, Workspace: "/workspace"},
		}, nil
	}}
	defer host.Close(ctx)
	if _, err := host.Activate(ctx, "missing", true); err == nil {
		t.Fatal("missing v4 journal resumed as a new Session")
	}
	if _, found := host.Session("missing"); found || store.header.ID != "" {
		t.Fatalf("missing resume published an Agent or journal: found=%t header=%+v", found, store.header)
	}
	if _, err := host.Activate(ctx, "missing", false); err != nil {
		t.Fatalf("explicit creation after failed resume: %v", err)
	}
}
