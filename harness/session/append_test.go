package sessionformat

import (
	"encoding/json"
	"testing"
)

func TestAppendBatchValidationIsAtomic(t *testing.T) {
	s, err := NewLiveSession(Header{Type: "session", Version: 4, CreatedAt: 1, ID: "atomic", CWD: "/tmp"}, &memoryV4Journal{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	title := EventInput{Type: "session/title", Data: json.RawMessage(`{"title":"report"}`)}
	before := len(s.Events())
	if _, err := s.AppendBatch([]EventInput{title, {Type: "user/message", Data: json.RawMessage(`{}`)}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	if len(s.Events()) != before {
		t.Fatal("invalid batch committed a partial prefix")
	}
	if events, err := s.AppendBatch([]EventInput{title, title}); err != nil || len(events) != 2 {
		t.Fatal(events, err)
	}
}
