package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/interaction"
	"go-bot/harness/session"
	"go-bot/internal/hostruntime"
)

func TestHostStreamPublishesExtensionInvalidationWithoutASession(t *testing.T) {
	var mu sync.Mutex
	changed := make(chan struct{})
	host := &hostruntime.NativeHost{PluginsChanges: func() (<-chan struct{}, <-chan struct{}) {
		mu.Lock()
		defer mu.Unlock()
		return changed, t.Context().Done()
	}}
	server := httptest.NewServer(NewNativeHostHTTP(host, NativeWorkerHTTPOptions{}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/host/stream", nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewScanner(response.Body)
	next := func() {
		t.Helper()
		for reader.Scan() {
			data, ok := strings.CutPrefix(reader.Text(), "data: ")
			if !ok {
				continue
			}
			var event hostStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "extensions.changed" {
				continue
			}
			if event.SessionID != "" {
				t.Fatal("extension invalidation was attributed to a chat")
			}
			return
		}
		t.Fatalf("no extension event: %v", reader.Err())
	}
	next()
	mu.Lock()
	old := changed
	changed = make(chan struct{})
	close(old)
	mu.Unlock()
	next()
}

type hostStreamQuestionWorker struct{ *nativeHTTPWorker }

func (*hostStreamQuestionWorker) PendingQuestions() ([]interaction.Pending, error) {
	return []interaction.Pending{{CallID: "pending", Turn: 1, Step: 1, Questions: []interaction.Question{{ID: "choice", Question: "Choose?"}}}}, nil
}

func TestHostSessionFollowerRestoresControlsAndFiltersPrivateContent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker := &hostStreamQuestionWorker{&nativeHTTPWorker{
		events: []sessionformat.Event{
			{Seq: 0, Type: "turn/start", Data: json.RawMessage(`{"turn":1}`)},
			{Seq: 1, Type: "system/message", Data: json.RawMessage(`{"secret":"private-system"}`)},
			{Seq: 2, Type: "turn/end", Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
		},
		baseline: agent.NativeAssistantStreamBaseline{Revision: 2, ActiveAttempt: &agent.NativeAssistantStreamAttemptBaseline{
			AttemptID: "active", NextIndex: 1, Stream: []any{map[string]any{"type": "reasoning-chunks", "texts": []any{"private-reasoning"}}},
		}},
		stream: make(chan agent.NativeAssistantStreamFrame), done: make(chan struct{}),
	}}
	type result struct {
		kind string
		data []byte
	}
	output := make(chan result, 16)
	done := make(chan error, 1)
	after := uint64(1)
	go func() {
		done <- followHostSession(ctx, worker, &after, true, func(kind string, data any) error {
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			output <- result{kind, encoded}
			return nil
		})
	}()
	questions, replay, state := false, false, false
	for {
		select {
		case event := <-output:
			if strings.Contains(string(event.data), "private-") {
				t.Fatalf("private content escaped: %s", event.data)
			}
			switch event.kind {
			case "session":
				replay = strings.Contains(string(event.data), `"seq":2`)
			case "questions.state":
				questions = strings.Contains(string(event.data), `"callId":"pending"`)
			case "session.state":
				state = strings.Contains(string(event.data), `"activeTurn":null`)
			case "session.cursor":
				if !questions || !replay || !state {
					t.Fatalf("incomplete snapshot: questions=%t replay=%t state=%t", questions, replay, state)
				}
				cancel()
				select {
				case <-done:
					return
				case <-time.After(time.Second):
					t.Fatal("follower did not cancel")
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("snapshot did not arrive")
		}
	}
}

func TestHostStreamValidatesSessionCursorsBeforeOpeningAudience(t *testing.T) {
	for _, query := range []string{"cursors=broken", "cursors=%7B%22not-a-session%22%3A1%7D", "cursors=%7B%22b231fb9e-976a-42c6-b191-a4bf627c88bd%22%3A-1%7D"} {
		host := &hostruntime.NativeHost{}
		out := httptest.NewRecorder()
		streamHostEvents(out, httptest.NewRequest(http.MethodGet, "/v1/host/stream?"+query, nil), host)
		if out.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d", query, out.Code)
		}
	}
}
