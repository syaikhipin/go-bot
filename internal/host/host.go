// Package host composes a single personal computer with many durable v4 sessions.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/assembly"
	"go-bot/harness/attachments"
	"go-bot/harness/memory"
	"go-bot/harness/schedule"
	"go-bot/harness/session"
	"go-bot/harness/sessionsearch"
	"go-bot/harness/skills"
	"go-bot/harness/tools"
	"go-bot/internal/config"
	"go-bot/internal/durablefs"
	"go-bot/internal/executor"
	"go-bot/internal/hostruntime"
	"go-bot/internal/httpapi"
	"go-bot/internal/modelsettings"
	"go-bot/internal/websearchsettings"
	imageproviders "go-bot/providers/image"
	"go-bot/providers/model/mainmodel"
	webproviders "go-bot/providers/web"
	builtin "go-bot/providers/web/builtin"
	"go-bot/thirdparty/llm"
	"go-bot/web"
)

type Ready struct {
	RunID      string `json:"runId"`
	APIVersion string `json:"apiVersion"`
}

// Run starts a Controller-managed Host after validating its startup configuration.
func Run(ctx context.Context, c config.Config) error {
	if err := c.ValidateHost(); err != nil {
		return err
	}
	return run(ctx, c, func(options web.Options) (web.SearchProvider, error) {
		return webproviders.New(webproviders.Default, options)
	}, builtin.Fetcher{})
}

func sessionLogRoot(dataRoot string) string {
	return filepath.Join(dataRoot, skills.UserDSHHomeDir, "sessions")
}

func run(ctx context.Context, c config.Config, newSearchProvider web.Factory, fetch tools.WebFetcher) error {
	root := config.DataRoot
	if err := durablefs.EnsureDirectory(root); err != nil {
		return err
	}
	mode := "container"
	if c.Host.DataMounted {
		mode = "mounted"
	}
	builder, err := executor.NewVerifiedBuilder(mode)
	if err != nil {
		return err
	}
	if err := skills.Bootstrap(ctx, root, skills.SeedDir); err != nil {
		return fmt.Errorf("initialize Host skills: %w", err)
	}
	search := &hostWebSearch{defaultFactory: newSearchProvider}
	imageGeneration := &hostImageGeneration{}
	dshHome := filepath.Join(root, skills.UserDSHHomeDir)
	images := attachments.Store{API: attachments.LocalObjectAPI{Root: dshHome}}
	files := attachments.FileStore{Root: dshHome}
	requestImages := &attachments.RequestImages{Store: images, MaxWidth: 2048, MaxHeight: 2048, MaxBytes: 4 << 20}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The agent deliberately uses the deployment's configured HTTP(S) proxy.
	defer transport.CloseIdleConnections()
	model := mainmodel.Model{Images: requestImages, ImageAccessPath: images.ImageHostPath, Transport: transport}
	models := &hostModels{base: model, snapshot: modelsettings.Snapshot{Revision: -1}}
	if err := models.loadReservations(filepath.Join(dshHome, "storages", "scheduled-models")); err != nil {
		return err
	}
	builder.WebFetcher = fetch
	builder.ImageAttachments = images
	builder.ImageInputEnabled = true
	builder.WebSearcher = search
	builder.ImageGenerator = imageGeneration
	native := &hostruntime.NativeHost{UserID: c.Host.AccountID, Mode: "host", StartedAt: time.Now().UTC(), ReserveScheduleModel: models.reserve}
	scheduleStore := schedule.FileStore{Path: filepath.Join(dshHome, "storages", "schedule.json")}
	native.Schedule, err = schedule.NewHostService(scheduleStore, native.PrepareSchedule, native.DeliverSchedule)
	if err != nil {
		return err
	}
	native.Schedule.SetDestinationCreator(native.EnsureScheduleDestination)
	claims := &sessionformat.ProcessClaims{}
	atWorkspace := func(cwd string) sessionformat.FileStore {
		return sessionformat.FileStore{Root: sessionLogRoot(root), ProjectCWD: cwd, Claim: claims.Acquire}
	}
	resolveStore := func(ctx context.Context, sid string) (sessionformat.FileStore, error) {
		header, err := findHostSession(ctx, root, sid)
		return atWorkspace(header.CWD), err
	}
	native.WorkspaceForSession = func(sid string) string {
		header, _ := findHostSession(context.Background(), root, sid)
		return header.CWD
	}
	native.Archived = func(sid string) (bool, error) {
		store, err := resolveStore(context.Background(), sid)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return archivedChat(store, sid)
	}
	native.SaveArchive = func(ctx context.Context, sid string) error {
		store, err := resolveStore(ctx, sid)
		if err != nil {
			return err
		}
		return archiveChat(ctx, store, sid)
	}
	native.ReadTitle = func(sid string) string {
		store, err := resolveStore(context.Background(), sid)
		if err != nil {
			return ""
		}
		return chatTitle(store, sid)
	}
	native.SaveTitle = func(sid, title string) error {
		store, err := resolveStore(context.Background(), sid)
		if err != nil {
			return err
		}
		return saveChatTitle(store, sid, title)
	}
	native.Schedule.SetSessionTargetCheck(native.ValidateScheduleDestination)
	native.Schedule.ConfigureDelivery(schedule.Limits{
		HistoryDays: c.Schedule.DeliveryHistoryDays, HistoryRecords: c.Schedule.DeliveryHistoryRecords,
		NewSessionIntervalSeconds: c.Schedule.NewSessionIntervalSeconds,
	}, native.ObserveScheduleExecution, native.NotifyScheduleChange)
	native.ReadSessionHeader = func(ctx context.Context, sid string) (sessionformat.Header, error) {
		return findHostSession(ctx, root, sid)
	}
	native.SessionLogInfo = func(sid string) (os.FileInfo, error) {
		store, err := resolveStore(context.Background(), sid)
		if err != nil {
			return nil, err
		}
		path, err := store.Path(sid)
		if err != nil {
			return nil, err
		}
		return os.Stat(path)
	}
	native.ReadSession = func(ctx context.Context, sid string) (sessionformat.PhysicalArtifact, error) {
		store, err := resolveStore(ctx, sid)
		if err != nil {
			return sessionformat.PhysicalArtifact{}, err
		}
		return store.ReadArtifact(ctx, sid)
	}
	native.List = func(ctx context.Context, cursor string, limit int) (sessionformat.SessionListPage, error) {
		return listHostSessions(ctx, root, cursor, limit)
	}
	native.LatestSession = func(ctx context.Context) (sessionformat.SessionListPage, error) {
		return latestHostSession(ctx, root)
	}
	accountMemory := memory.NewStore(filepath.Join(dshHome, "memories"))
	history, err := sessionsearch.Open(ctx, "/var/lib/go-bot/session-search.sqlite", c.Host.AccountID, sessionsearch.Source{
		List: native.List, Snapshot: historySnapshot(native), Title: native.ReadTitle,
	})
	if err != nil {
		return fmt.Errorf("open Host session search index: %w", err)
	}
	defer history.Close()
	if err := history.Sync(ctx); err != nil {
		return fmt.Errorf("sync Host session search index: %w", err)
	}
	buildRuntime := func(sid, workspace, preset string) (*hostruntime.NativeRuntime, error) {
		return &hostruntime.NativeRuntime{
			Factory: assembly.NativeFactory{
				Storage: executor.NativeFileStorage{Store: atWorkspace(workspace), SessionID: sid},
				Builder: builder, Model: model, ResolveTurnModel: models.resolver(sid),
				Provider: "unconfigured", ModelID: "unconfigured", SystemPrompt: assembly.DefaultCoreSystemPrompt,
				Limits: agent.Limits{MaxSteps: 128, MaxToolCalls: 256, MaxModelCalls: 160, MaxParallelToolCalls: 4,
					ContextWindowTokens: config.ModelContextWindow, ReservedOutputTokens: 16 * 1024, ContextHeadroomTokens: 8 * 1024},
				Schedule: native.Schedule, FileAttachments: files, Memory: accountMemory, SessionSearch: history,
			},
			Identity:         assembly.NativeRuntimeIdentity{UserID: c.Host.AccountID, SessionID: sid, Workspace: workspace, AgentPreset: preset},
			HostOwnsSchedule: true,
		}, nil
	}
	native.BuildInWorkspace = func(sid string, create schedule.TargetCreate) (*hostruntime.NativeRuntime, error) {
		workspace := create.CWD
		if workspace == "" {
			workspace = root
		}
		workspace = filepath.Clean(workspace)
		relative, err := filepath.Rel(root, workspace)
		if err != nil || !filepath.IsLocal(relative) {
			return nil, schedule.ErrInvalid
		}
		if err := durablefs.EnsureDirectory(workspace); err != nil {
			return nil, err
		}
		return buildRuntime(sid, workspace, create.AgentPreset)
	}
	native.Build = func(sid string, resume bool) (*hostruntime.NativeRuntime, error) {
		if resume {
			header, err := findHostSession(context.Background(), root, sid)
			if err != nil {
				return nil, err
			}
			return buildRuntime(sid, header.CWD, header.AgentPreset)
		}
		return native.BuildInWorkspace(sid, schedule.TargetCreate{CWD: defaultSessionWorkspace(root, sid)})
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err = native.Schedule.Start(runCtx); err != nil {
		return err
	}
	base := httpapi.NewNativeHostHTTP(native, httpapi.NativeWorkerHTTPOptions{
		HostFilesRoot: root,
		ReadSchedules: scheduleStore.Load,
		Images:        images, Files: files, ImageInputEnabled: true,
	})
	var draining atomic.Bool
	ready := Ready{RunID: c.Host.RunID, APIVersion: "v1"}
	wakeForModelChange := func() {
		native.WakeSessions()
		native.Schedule.NotifyModelChange()
	}
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case websearchsettings.InternalPath:
			search.handle(w, r)
		case imageproviders.InternalPath:
			imageGeneration.handle(w, r)
		case modelsettings.InternalPath:
			models.handle(w, r, wakeForModelChange)
		case "/readyz":
			if draining.Load() {
				http.Error(w, "draining", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ready)
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(map[string]any{"model": models.defaultModel(), "protocol": llm.Responses, "search": search.configured(), "apiVersion": "v1"})
		case "/internal/host/shutdown":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			draining.Store(true)
			w.WriteHeader(202)
			cancel()
		default:
			if id, ok := strings.CutPrefix(r.URL.Path, modelsettings.ScheduledSelectionPrefix); ok {
				models.handleScheduledSelection(w, r, id, wakeForModelChange)
				return
			}
			base.ServeHTTP(w, r)
		}
	})
	server := &http.Server{Handler: httpapi.Authenticate(c.Auth.HostToken, mux), BaseContext: func(net.Listener) context.Context { return runCtx }, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", config.HostListen)
	if err != nil {
		native.Schedule.Close()
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	fmt.Printf("Host ready: %s (run=%s)\n", listener.Addr(), ready.RunID)
	var runErr error
	select {
	case <-runCtx.Done():
	case runErr = <-serveErr:
	}
	draining.Store(true)
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	if e := server.Shutdown(shutdown); e != nil {
		_ = server.Close()
		runErr = errors.Join(runErr, e)
	}
	if errors.Is(runErr, http.ErrServerClosed) {
		runErr = nil
	}
	return errors.Join(runErr, native.Close(shutdown))
}
