package session

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadLineLimitedRejectsOversizedCompleteAndTornLines(t *testing.T) {
	for _, input := range []string{"123456789\n", "123456789"} {
		reader := bufio.NewReaderSize(strings.NewReader(input), 4)
		if _, err := readLineLimited(reader, 8); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized line %q was accepted: %v", input, err)
		}
	}
	reader := bufio.NewReaderSize(strings.NewReader("1234567\nnext\n"), 4)
	line, err := readLineLimited(reader, 8)
	if err != nil || string(line) != "1234567\n" {
		t.Fatalf("bounded line = %q, %v", line, err)
	}
	line, err = readLineLimited(reader, 8)
	if err != nil || string(line) != "next\n" {
		t.Fatalf("subsequent line was lost = %q, %v", line, err)
	}
}

func TestJournalEncoderRejectsLineBeyondReaderLimit(t *testing.T) {
	data := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, maxStoredLineBytes)...)
	data = append(data, '"')
	row := storedLine{Kind: "event", Event: &Event{Seq: 1, Type: "user/message", At: time.Now().UTC(), Data: data}}
	if _, err := encodeStoredLine(row); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized event was encoded: %v", err)
	}
}

func TestSessionRejectsOversizedEventWithoutPoisoningNextAppend(t *testing.T) {
	s, err := New(testHeader(), &failingJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, strings.Repeat("x", maxStoredLineBytes)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized in-memory event accepted: %v", err)
	}
	if _, err := s.Append("user/message", 1, 0, "small"); err != nil {
		t.Fatalf("size rejection poisoned Session: %v", err)
	}
	if events := s.Events(); len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("size rejection changed event sequence: %#v", events)
	}
}

func TestEventTailIsDetachedAndRejectsFutureCursor(t *testing.T) {
	s, err := New(testHeader(), &failingJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, map[string]string{"content": "hello"}); err != nil {
		t.Fatal(err)
	}
	tail, through, err := s.EventTail(0)
	if err != nil || through != 1 || len(tail) != 1 {
		t.Fatalf("event tail = %#v through=%d err=%v", tail, through, err)
	}
	tail[0].Data[0] = 'x'
	again, _, err := s.EventTail(0)
	if err != nil || again[0].Data[0] != '{' {
		t.Fatalf("caller mutated stored event through tail: %#v, %v", again, err)
	}
	if _, _, err := s.EventTail(2); err == nil {
		t.Fatal("future event-tail cursor accepted")
	}
}

func TestLatestEventOfTypeTracksAppendsAndResumeWithoutAliasing(t *testing.T) {
	header := testHeader()
	s, err := New(header, activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := s.LatestEventOfType("skill/catalog"); found {
		t.Fatal("empty Session reported a Skill catalog")
	}
	for _, item := range []struct {
		kind string
		text string
	}{
		{"skill/catalog", "first"},
		{"tool/result", "unrelated"},
		{"skill/catalog", "second"},
	} {
		if _, err := s.Append(item.kind, 0, 0, map[string]string{"text": item.text}); err != nil {
			t.Fatal(err)
		}
	}
	latest, found := s.LatestEventOfType("skill/catalog")
	if !found || latest.Seq != 3 || !bytes.Contains(latest.Data, []byte("second")) {
		t.Fatalf("latest Skill catalog = %#v, found=%t", latest, found)
	}
	latest.Data[0] = 'x'
	if again, found := s.LatestEventOfType("skill/catalog"); !found || again.Data[0] != '{' {
		t.Fatalf("caller changed indexed event: %#v, found=%t", again, found)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, err := Resume(header, s.DurableEvents(), activityJournal{})
	if err != nil {
		t.Fatal(err)
	}
	if fromHistory, found := restored.LatestEventOfType("skill/catalog"); !found || fromHistory.Seq != 3 {
		t.Fatalf("resume lost latest catalog: %#v, found=%t", fromHistory, found)
	}
	if _, err := restored.Append("skill/catalog", 0, 0, map[string]string{"text": "third"}); err != nil {
		t.Fatal(err)
	}
	if appended, found := restored.LatestEventOfType("skill/catalog"); !found || appended.Seq != 4 || !bytes.Contains(appended.Data, []byte("third")) {
		t.Fatalf("post-resume append did not advance index: %#v, found=%t", appended, found)
	}
}

func testHeader() Header {
	return Header{
		SchemaVersion: SchemaVersion,
		SessionID:     "session-123",
		UserID:        "user-123",
		Mode:          "container",
		Workspace:     "/workspace",
		CreatedAt:     time.Now().UTC(),
	}
}

func TestFileJournalFlushAndResume(t *testing.T) {
	ctx := context.Background()
	guard := &ProcessClaims{}
	store := FileStore{Root: t.TempDir(), Claim: guard.Acquire}
	header := testHeader()
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Open(ctx, header.UserID, header.SessionID); err == nil {
		t.Fatal("second writer acquired an active session")
	}
	live, err := New(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Append("user/message", 1, 0, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := live.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Append("turn/start", 1, 0, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	reopened, gotHeader, events, err := store.Open(ctx, header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if gotHeader.SessionID != header.SessionID || len(events) != 2 || events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("recovered header/events: %#v %#v", gotHeader, events)
	}
	resumed, err := Resume(gotHeader, events, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Append("turn/end", 1, 0, map[string]string{"reason": "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileOpenRejectsCancelledRecoveryBeforeReading(t *testing.T) {
	claims := &ProcessClaims{}
	store := FileStore{Root: t.TempDir(), Claim: claims.Acquire}
	header := testHeader()
	journal, err := store.Create(context.Background(), header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := store.Open(ctx, header.UserID, header.SessionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled FileStore recovery = %v", err)
	}
	opened, _, _, err := store.Open(context.Background(), header.UserID, header.SessionID)
	if err != nil {
		t.Fatalf("cancelled read retained write claim: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileJournalTruncatesTornTailButRejectsCompleteCorruption(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	header := testHeader()
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := store.path(header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"kind":"event","event":{"seq":1`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, events, err := store.Open(ctx, header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events after torn-tail repair: %d", len(events))
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 {
		t.Fatalf("torn tail remained: %q", data)
	}
	if err := os.WriteFile(path, append(data, []byte("not-json\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Open(ctx, header.UserID, header.SessionID); err == nil {
		t.Fatal("complete corrupt line must not be silently discarded")
	}
}

func TestReadSnapshotDoesNotClaimOrRepair(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	header := testHeader()
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := store.path(header.UserID, header.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	torn := []byte(`{"kind":"event","event":{"seq":1`)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(torn); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reader := FileStore{Root: store.Root}
	selected, err := reader.ReadHeader(header.UserID, header.SessionID)
	if err != nil || selected.Mode != header.Mode {
		t.Fatalf("header-only read = %#v, %v", selected, err)
	}
	if _, err := reader.ReadHeader("other-user", header.SessionID); err == nil {
		t.Fatal("another user's Session header was exposed")
	}
	gotHeader, events, err := reader.ReadSnapshot(header.UserID, header.SessionID)
	if err != nil || gotHeader.SessionID != header.SessionID || len(events) != 0 {
		t.Fatalf("snapshot = %#v %#v %v", gotHeader, events, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(data), string(torn)) {
		t.Fatalf("snapshot changed torn tail: %q, %v", data, err)
	}
}

func TestFileStoreRejectsUnsafeIdentityAndMissingClaim(t *testing.T) {
	root := t.TempDir()
	store := FileStore{Root: root, Claim: (&ProcessClaims{}).Acquire}
	if _, err := store.path("../user", "session"); err == nil {
		t.Fatal("unsafe path accepted")
	}
	if _, err := (FileStore{Root: root}).path("user", "session"); err == nil {
		t.Fatal("missing owner guard accepted")
	}
	if _, err := (FileStore{Root: "relative", Claim: store.Claim}).path("user", "session"); err == nil {
		t.Fatal("relative mount root accepted")
	}
	if got := filepath.Base(root); got == "" {
		t.Fatal("unexpected empty test root")
	}
}

type failingJournal struct{ appended int }

func (j *failingJournal) Append(_ context.Context, events []Event) error {
	j.appended += len(events)
	return errors.New("ambiguous write")
}
func (*failingJournal) Flush(context.Context) error { return nil }
func (*failingJournal) Close() error                { return nil }

func TestSessionPoisonedAfterAmbiguousAppend(t *testing.T) {
	j := &failingJournal{}
	s, err := New(testHeader(), j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); !errors.Is(err, ErrUncertainWrite) {
		t.Fatalf("flush error = %v", err)
	}
	if _, err := s.Append("tool/start", 1, 1, nil); !errors.Is(err, ErrUncertainWrite) {
		t.Fatalf("append after uncertain write = %v", err)
	}
	if j.appended != 1 {
		t.Fatalf("ambiguous batch retried %d times", j.appended)
	}
}

func TestFileJournalRejectsWholeInvalidBatchBeforeWriting(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	header := testHeader()
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	valid := Event{Seq: 1, Type: "user/message", At: now, Data: []byte(`{"content":"first"}`)}
	invalid := Event{Seq: 3, Type: "assistant/message", At: now}
	if err := journal.Append(ctx, []Event{valid, invalid}); err == nil {
		t.Fatal("invalid second event was accepted")
	}
	if err := journal.Append(ctx, []Event{valid}); err != nil {
		t.Fatalf("invalid batch advanced the journal sequence: %v", err)
	}
	if err := journal.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	_, events, err := store.ReadSnapshot(header.UserID, header.SessionID)
	if err != nil || len(events) != 1 || events[0].Seq != 1 {
		t.Fatalf("invalid batch left a file prefix: %#v, %v", events, err)
	}
}
