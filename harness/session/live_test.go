package sessionformat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLiveFileStore(t *testing.T) FileStore {
	t.Helper()
	return FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: "/root/harness/workspaces/session", Claim: v4Claim}
}

func readLiveFileArtifact(t *testing.T, ctx context.Context, store FileStore, id string) PhysicalArtifact {
	t.Helper()
	journal, artifact, err := store.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	return artifact
}

type uncertainFileJournal struct{ *FileJournal }

func (j uncertainFileJournal) Append(ctx context.Context, events []Event) error {
	if err := j.FileJournal.Append(ctx, events); err != nil {
		return err
	}
	return ErrV4UncertainWrite // simulate a committed batch with lost acknowledgement
}

func TestLiveSessionAcceptsInMemoryThenCheckpointsNativeV4(t *testing.T) {
	ctx := context.Background()
	store := newLiveFileStore(t)
	header := v4TestHeader("session")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	userMessage := json.RawMessage(`{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"hello"}]}`)
	accepted, err := s.Append(EventInput{Type: "user/message", Data: userMessage, SurfaceOp: json.RawMessage(`"append"`)})
	if err != nil || accepted.Seq != 0 {
		t.Fatalf("append = %+v, %v", accepted, err)
	}
	userMessage[0] = 'X'
	if len(s.DurableEvents()) != 0 || len(s.Events()) != 1 {
		t.Fatal("in-memory append crossed the durability checkpoint")
	}
	surface, err := s.Surface()
	if err != nil || len(surface.Messages) != 1 || !strings.Contains(string(surface.Messages[0]), `"hello"`) {
		t.Fatalf("live surface = %+v, %v", surface, err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.DurableEvents()) != 1 {
		t.Fatal("checkpoint did not publish the durable prefix")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(artifact.Events) != 1 || string(artifact.Events[0].Data) != `{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"hello"}]}` {
		t.Fatalf("v4 artifact changed after caller mutation: %+v", artifact.Events)
	}
}

func TestLiveSessionDurableEventsFromReturnsDetachedCommittedSuffix(t *testing.T) {
	s, err := NewLiveSession(v4TestHeader("suffix"), &memoryV4Journal{})
	if err != nil {
		t.Fatal(err)
	}
	message := json.RawMessage(`{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"hello"}]}`)
	if _, err := s.Append(EventInput{Type: "user/message", Data: message, SurfaceOp: json.RawMessage(`"append"`)}); err != nil {
		t.Fatal(err)
	}
	if suffix, err := s.DurableEventsFrom(0); err != nil || len(suffix) != 0 {
		t.Fatalf("unflushed suffix = %+v, %v", suffix, err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	suffix, err := s.DurableEventsFrom(0)
	if err != nil || len(suffix) != 1 || suffix[0].Seq != 0 {
		t.Fatalf("committed suffix = %+v, %v", suffix, err)
	}
	suffix[0].Data[0] = 'X'
	if current := s.DurableEvents(); current[0].Data[0] != '{' {
		t.Fatal("suffix changed the live event")
	}
	if empty, err := s.DurableEventsFrom(1); err != nil || len(empty) != 0 {
		t.Fatalf("end cursor = %+v, %v", empty, err)
	}
	if _, err := s.DurableEventsFrom(2); err == nil {
		t.Fatal("future cursor was accepted")
	}
}

func TestRestoreLiveSessionStartsNewLifecycleAndKeepsSurface(t *testing.T) {
	ctx := context.Background()
	store := newLiveFileStore(t)
	header := v4TestHeader("session")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(ctx, []Event{{Type: "user/message", Seq: 0, Time: 1, Data: json.RawMessage(`{"id":"u1","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"old"}]}`), SurfaceOp: json.RawMessage(`"append"`)}}); err != nil {
		t.Fatal(err)
	}
	journal.Close()
	reopened, artifact, err := store.Open(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	s, err := RestoreLiveSession(artifact, reopened)
	if err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	if len(s.DurableEvents()) != 1 || len(s.Events()) != 2 || s.Events()[1].Type != "session/end-seed" {
		t.Fatalf("restore lifecycle boundary = %+v", s.Events())
	}
	surface, err := s.Surface()
	if err != nil || len(surface.Messages) != 1 || !strings.Contains(string(surface.Messages[0]), `"old"`) {
		t.Fatalf("restored surface = %+v, %v", surface, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	final := readLiveFileArtifact(t, ctx, store, "session")
	if len(final.Events) != 2 || final.Events[1].Type != "session/end-seed" {
		t.Fatalf("restored lifecycle was not persisted: %+v", final.Events)
	}
}

func TestRestoreLiveSessionClosesInterruptedTurnBeforeResumeMarker(t *testing.T) {
	ctx := context.Background()
	store := newLiveFileStore(t)
	header := v4TestHeader("interrupted-resume")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	const lastStoredTime = int64(123456)
	stored := []Event{
		{Type: "turn/start", Seq: 0, Time: lastStoredTime - 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: "step/start", Seq: 1, Time: lastStoredTime, Data: json.RawMessage(`{"turn":1,"step":1}`)},
	}
	if err := journal.Append(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, header.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := RestoreLiveSession(artifact, reopened)
	if err != nil {
		_ = reopened.Close()
		t.Fatal(err)
	}
	events := s.Events()
	if len(events) != 5 || events[2].Type != "step/end" || events[3].Type != "turn/end" || events[4].Type != "session/end-seed" {
		t.Fatalf("interrupted resume order = %+v", events)
	}
	if events[2].Time != lastStoredTime || events[3].Time != lastStoredTime {
		t.Fatalf("closers did not reuse the last stored timestamp: %+v", events[2:4])
	}
	if len(s.DurableEvents()) != len(stored) {
		t.Fatal("synthetic closers were claimed durable before checkpoint")
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	final := readLiveFileArtifact(t, ctx, store, header.ID)
	if len(final.Events) != len(events) {
		t.Fatalf("persisted resume events = %+v", final.Events)
	}
	if err := ValidateV4Relationships(final.Events); err != nil {
		t.Fatalf("persisted resume relationships: %v", err)
	}
}

func TestLiveSessionFailedCheckpointPoisonsOnlyUncertainWriter(t *testing.T) {
	ctx := context.Background()
	store := newLiveFileStore(t)
	header := v4TestHeader("session")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewLiveSession(header, uncertainFileJournal{journal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(EventInput{Type: "turn/start", Data: json.RawMessage(`{"turn":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); !errors.Is(err, ErrV4UncertainWrite) {
		t.Fatalf("lost checkpoint acknowledgement = %v", err)
	}
	if len(s.DurableEvents()) != 0 {
		t.Fatal("uncertain checkpoint was published as durable")
	}
	if _, err := s.Append(EventInput{Type: "turn/end", Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}); !errors.Is(err, ErrV4UncertainWrite) {
		t.Fatalf("poisoned session accepted more work: %v", err)
	}
	_ = s.Close()
	reopened, artifact, err := store.Open(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(artifact.Events) != 1 || artifact.Events[0].Type != "turn/start" {
		t.Fatalf("committed event not recovered: %+v", artifact.Events)
	}
}

func TestRestoreLiveSessionRejectsMissingInheritedCut(t *testing.T) {
	ctx := context.Background()
	store := newLiveFileStore(t)
	store.RootSessionID = "root"
	header := v4TestHeader("child")
	header.CWD = store.ProjectCWD
	header.IsSeeded = true
	header.ParentSession = "root"
	header.Origin = "subagent"
	header.DelegationDepth = 1
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := RestoreLiveSession(PhysicalArtifact{Header: header, InheritedEventCount: 0}, journal); err == nil {
		t.Fatal("seeded Session without its inherited marker was restored")
	}
}

func TestLiveSessionTakesLogicalSourceSeqsAndWritesPhysicalRange(t *testing.T) {
	ctx := context.Background()
	store := newLiveFileStore(t)
	header := v4TestHeader("session")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		message := json.RawMessage(`{"id":"u","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"old"}]}`)
		if _, err := s.Append(EventInput{Type: "user/message", Data: message, SurfaceOp: json.RawMessage(`"append"`)}); err != nil {
			t.Fatal(err)
		}
	}
	sources := []uint64{0, 1, 2}
	replacement, err := s.Append(EventInput{
		Type: "user/message", Data: json.RawMessage(`{"id":"summary","role":"user","source":{"kind":"compact-checkpoint"},"content":[{"type":"text","text":"summary"}]}`),
		SurfaceOp: json.RawMessage(`{"op":"replace","startSeq":0,"endSeq":2}`), SourceEventSeqs: sources,
	})
	if err != nil || string(replacement.SourceEventSeqs) != `[[0,2]]` {
		t.Fatalf("logical source encoding = %+v, %v", replacement, err)
	}
	sources[0] = 99
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// This test asserts the physical range encoding, not a complete
	// compaction lifecycle; the production FileStore.Open rejects that partial
	// semantic trace by design.
	path, err := store.Path("session")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := DecodePhysicalJSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Events) != 4 || string(artifact.Events[3].SourceEventSeqs) != `[[0,2]]` {
		t.Fatalf("physical source encoding = %+v", artifact.Events)
	}
}
