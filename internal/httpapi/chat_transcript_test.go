package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"go-bot/harness/agent"
	"go-bot/harness/assembly"
	"go-bot/harness/attachments"
	"go-bot/harness/session"
	"go-bot/internal/hostruntime"
)

func TestChatToolImagesInStreamAndHistory(t *testing.T) {
	store := attachments.Store{API: &nativeImageObjects{}}
	original := nativeTinyPNG(t)
	ref, err := store.SaveImage(t.Context(), original, "image/png", "read.png", "")
	if err != nil {
		t.Fatal(err)
	}
	image := chatAttachment{
		FileRef:   attachments.FileRef{ID: ref.ID, Name: ref.Name, Bytes: ref.Bytes},
		MediaType: ref.MIMEType, Width: ref.Width, Height: ref.Height,
	}
	result, err := json.Marshal(map[string]any{"message": map[string]any{
		"id": "result", "toolCallId": "read", "content": []any{
			map[string]string{"type": "text", "text": "private tool output"},
			chatBlock{Type: "image", Attachment: &image},
			chatBlock{Type: "image", Attachment: &chatAttachment{FileRef: attachments.FileRef{ID: "invalid"}}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	events := []sessionformat.Event{
		{Seq: 0, Type: "tool/result", Data: result, SurfaceOp: json.RawMessage(`"append"`)},
		{Seq: 1, Type: "tool/result", Data: json.RawMessage(`{"message":{"id":"result","toolCallId":"read","content":[{"type":"text","text":"pruned"}]}}`),
			SurfaceOp: json.RawMessage(`{"op":"replace","startSeq":0,"endSeq":0}`)},
	}
	id := uuid.NewString()
	path := "/v1/sessions/" + id
	options := NativeWorkerHTTPOptions{Images: store}
	frames := make(chan agent.NativeAssistantStreamFrame)
	close(frames)
	worker := &nativeHTTPWorker{events: events, stream: frames}
	live := NewNativeWorkerHTTP(worker, assembly.NativeRuntimeIdentity{UserID: "alice", SessionID: id}, options)
	stream := httptest.NewRecorder()
	live.ServeHTTP(stream, httptest.NewRequest(http.MethodGet, path+"/stream?view=chat", nil))

	cold := NewNativeHostHTTP(&hostruntime.NativeHost{ReadSession: func(context.Context, string) (sessionformat.PhysicalArtifact, error) {
		return sessionformat.PhysicalArtifact{Events: events}, nil
	}}, options)
	history := httptest.NewRecorder()
	cold.ServeHTTP(history, httptest.NewRequest(http.MethodGet, path+"/transcript", nil))
	var page chatTranscriptPage
	if err := json.Unmarshal(history.Body.Bytes(), &page); err != nil || history.Code != http.StatusOK || len(page.Events) != 1 {
		t.Fatalf("expected only the original tool result in history, got %s: %v", history.Body, err)
	}
	var data struct {
		CallID string
		Status string
		Images []chatBlock
	}
	if err := json.Unmarshal(page.Events[0].Data, &data); err != nil || data.CallID != "read" || data.Status != "succeeded" ||
		len(data.Images) != 1 || data.Images[0].ID != "result/1" || data.Images[0].Type != "image" ||
		data.Images[0].Attachment == nil || data.Images[0].Attachment.ID != ref.ID ||
		bytes.Contains(page.Events[0].Data, []byte("private tool output")) {
		t.Fatalf("unexpected projected result: %s (%v)", page.Events[0].Data, err)
	}
	eventJSON, err := json.Marshal(page.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	if stream.Code != http.StatusOK || strings.Count(stream.Body.String(), "event: session\n") != 1 ||
		!strings.Contains(stream.Body.String(), "event: session\ndata: "+string(eventJSON)+"\n") {
		t.Fatalf("stream differs from history: %s", stream.Body)
	}
	imageResponse := httptest.NewRecorder()
	cold.ServeHTTP(imageResponse, httptest.NewRequest(http.MethodGet,
		path+"/attachments/"+ref.ID+"?name="+url.QueryEscape(ref.Name), nil))
	if imageResponse.Code != http.StatusOK || imageResponse.Header().Get("Content-Type") != ref.MIMEType ||
		!bytes.Equal(imageResponse.Body.Bytes(), original) {
		t.Fatalf("attachment response: status %d, content type %s", imageResponse.Code, imageResponse.Header().Get("Content-Type"))
	}
}
