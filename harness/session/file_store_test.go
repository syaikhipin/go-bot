package sessionformat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendSyncedFileBatchRollsBackFailedCheckpoint(t *testing.T) {
	for _, test := range []struct {
		name  string
		write func(*os.File, []byte) (int, error)
		sync  func(*os.File) error
		want  error
	}{
		{name: "short write", write: func(file *os.File, data []byte) (int, error) {
			return file.Write(data[:len(data)/2])
		}, want: io.ErrShortWrite},
		{name: "fsync failure", sync: func(*os.File) error { return os.ErrPermission }, want: os.ErrPermission},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "session-v4-*")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			before := []byte("committed header and events\n")
			if _, err := file.Write(before); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := appendSyncedFileBatch(file, []byte("complete event row\n"), test.write, test.sync); !errors.Is(err, test.want) || !errors.Is(err, ErrV4WriteRolledBack) || errors.Is(err, ErrV4UncertainWrite) {
				t.Fatalf("failed checkpoint = %v, want %v", err, test.want)
			}
			data, err := os.ReadFile(file.Name())
			if err != nil || !bytes.Equal(data, before) {
				t.Fatalf("failed checkpoint left physical bytes: %v, %q", err, data)
			}
			if err := appendSyncedFileBatch(file, []byte("next committed row\n"), nil, nil); err != nil {
				t.Fatalf("append after rollback: %v", err)
			}
			data, err = os.ReadFile(file.Name())
			if err != nil || !bytes.Equal(data, append(before, []byte("next committed row\n")...)) {
				t.Fatalf("next checkpoint offset = %v, %q", err, data)
			}
		})
	}
}

func TestAppendSyncedFileBatchFailedRollbackRemainsUncertain(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "session-v4-*")
	if err != nil {
		t.Fatal(err)
	}
	err = appendSyncedFileBatch(file, []byte("unconfirmed row\n"), nil, func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) || !errors.Is(err, os.ErrClosed) || errors.Is(err, ErrV4WriteRolledBack) {
		t.Fatalf("failed rollback was misclassified as retryable: %v", err)
	}
}

func TestLiveFileCheckpointRetriesOnlyAfterConfirmedRollback(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: "/root/harness/workspaces/retry", Claim: v4Claim}
	header := v4TestHeader("retry")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	journal.appendBatch = func(file *os.File, data []byte) error {
		if !failed {
			failed = true
			return appendSyncedFileBatch(file, data, nil, func(*os.File) error { return os.ErrPermission })
		}
		return appendSyncedFileBatch(file, data, nil, nil)
	}
	live, err := NewLiveSession(header, journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Append(EventInput{Type: "turn/start", Data: json.RawMessage(`{"turn":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := live.Flush(ctx); !errors.Is(err, ErrV4WriteRolledBack) || errors.Is(err, ErrV4UncertainWrite) {
		t.Fatalf("rolled-back checkpoint = %v", err)
	}
	if len(live.DurableEvents()) != 0 {
		t.Fatal("rolled-back batch advanced the durable cursor")
	}
	if err := live.Flush(ctx); err != nil {
		t.Fatalf("retry on the same live Session: %v", err)
	}
	if len(live.DurableEvents()) != 1 {
		t.Fatal("retry did not advance the durable cursor exactly once")
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, header.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(artifact.Events) != 1 || artifact.Events[0].Seq != 0 || artifact.Events[0].Type != "turn/start" {
		t.Fatalf("retry duplicated the v4 batch: %+v", artifact.Events)
	}
}

func TestFileStorePersistsAndRepairsInterruptedTail(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/first"
	store := FileStore{Root: filepath.Join(t.TempDir(), ".dsh", "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("first")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if !strings.Contains(path, "/--root-harness-workspaces-first--/first/session.v4.jsonl") {
		t.Fatalf("wrong local Session path: %q", path)
	}
	first := Event{Type: "turn/start", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{"turn":1}`)}
	if err := journal.Append(ctx, []Event{first}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"turn/end"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Events) != 1 || artifact.Events[0].Type != "turn/start" {
		t.Fatalf("repaired artifact = %+v", artifact)
	}
	beforeAppend, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(beforeAppend), `{"type":"turn/end"`) {
		t.Fatalf("opening a torn log changed it before append: %v, %s", err, beforeAppend)
	}
	end := Event{Type: "turn/end", Seq: 1, Time: header.CreatedAt + 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}
	if err := reopened.Append(ctx, []Event{end}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), `{"type":"turn/end"`+"\n") || !strings.HasSuffix(string(contents), "\n") {
		t.Fatalf("local log retained partial tail: %s", contents)
	}
	if _, err := InspectV4JSONL(strings.NewReader(string(contents))); err != nil {
		t.Fatalf("local log is not canonical v4: %v", err)
	}
}

func TestFileStoreRetainsCompleteRowsFromTornToolBatchOnResume(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: "/root/harness/workspaces/torn-tool-batch", Claim: v4Claim}
	header := v4TestHeader("torn-tool-batch")
	header.CWD = store.ProjectCWD
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	// Model a crash after complete rows of one unacknowledged batch reached
	// storage, but while the following tool result was still being written.
	rows := []Event{
		{Type: "turn/start", Seq: 0, Time: 200, Data: json.RawMessage(`{"turn":1}`)},
		{Type: "step/start", Seq: 1, Time: 201, Data: json.RawMessage(`{"turn":1,"step":1}`)},
		{Type: "assistant/message", Seq: 2, Time: 202, Data: json.RawMessage(`{"turn":1,"step":1,"message":{"id":"a","role":"assistant","source":{"kind":"model","provider":"p","model":"m"},"content":[{"type":"tool-call","id":"c","name":"shell","arguments":"{}"}]},"stream":[]}`), SurfaceOp: json.RawMessage(`"append"`)},
		{Type: "tool/call", Seq: 3, Time: 203, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"c","name":"shell","arguments":"{}"}`)},
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range rows {
		line, err := EncodeEvent(event, event.Seq)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := file.WriteString(`{"type":"tool/result"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, artifact, err := store.Open(ctx, header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Events) != len(rows) || artifact.Events[3].Type != "tool/call" {
		t.Fatalf("complete rows from torn batch were discarded: %+v", artifact.Events)
	}
	s, err := RestoreLiveSession(artifact, reopened)
	if err != nil {
		_ = reopened.Close()
		t.Fatal(err)
	}
	events := s.Events()
	if len(events) != 8 || events[4].Type != "tool/result" || events[5].Type != "step/end" || events[6].Type != "turn/end" || events[7].Type != "session/end-seed" {
		t.Fatalf("torn tool batch recovery order = %+v", events)
	}
	if events[4].Time != rows[3].Time || !strings.Contains(string(events[4].Data), "TOOL_OUTCOME_UNKNOWN") {
		t.Fatalf("started tool outcome was not recovered: %+v", events[4])
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(contents), `{"type":"tool/result"`+"\n") {
		t.Fatalf("torn tool tail survived first append: %v, %s", err, contents)
	}
	inspection, err := InspectV4JSONL(bytes.NewReader(contents))
	if err != nil || len(inspection.Events) != len(events) {
		t.Fatalf("repaired physical log = %+v, %v", inspection, err)
	}
	if err := ValidateV4Relationships(inspection.Events); err != nil {
		t.Fatalf("repaired tool relationships: %v", err)
	}
}

func TestFileStoreKeepsChildSiblingAndRejectsOverwrite(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/root"
	parent := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("root")
	header.CWD = cwd
	journal, err := parent.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.Create(ctx, header); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate local Session create = %v", err)
	}
	child := parent
	child.RootSessionID = "root"
	childHeader := v4TestHeader("child")
	childHeader.CWD = cwd
	childHeader.ParentSession = "root"
	childHeader.Origin = "subagent"
	childHeader.DelegationDepth = 1
	childJournal, err := child.Create(ctx, childHeader)
	if err != nil {
		t.Fatal(err)
	}
	defer childJournal.Close()
	parentPath, _ := parent.Path("root")
	if filepath.Dir(filepath.Dir(parentPath)) != filepath.Dir(filepath.Dir(childJournal.Path())) {
		t.Fatalf("child is not sibling of parent: %q %q", parentPath, childJournal.Path())
	}
}

func TestFileStoreDoesNotPromoteParentLinkedHeaderToTopLevel(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/not-a-root"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("not-a-root")
	header.CWD = cwd
	header.ParentSession = "another-session"
	if _, err := store.Create(ctx, header); err == nil {
		t.Fatal("top-level storage created a header linked to a parent")
	}
	filename, err := store.Path(header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	line, err := EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadHeader(ctx, header.ID); err == nil {
		t.Fatal("top-level history listed a parent-linked header")
	}
	if journal, _, err := store.Open(ctx, header.ID); err == nil {
		_ = journal.Close()
		t.Fatal("top-level resume opened a parent-linked header")
	}
}

func TestFileStoreCreatePublishesCompleteHeaderWithoutTemporaryLink(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/atomic-create"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("atomic-create")
	header.CWD = cwd
	path, err := store.Path(header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// An interrupted staging write is not a canonical Session and must not
	// prevent a new creation of the same ID.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".session-create-abandoned"), []byte(`{"type":`), 0600); err != nil {
		t.Fatal(err)
	}
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".session-create-") && entry.Name() != ".session-create-abandoned" {
			t.Fatalf("new creation left a temporary link: %q", entry.Name())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("published header is incomplete: %q", raw)
	}
	if _, err := InspectV4JSONL(strings.NewReader(string(raw))); err != nil {
		t.Fatalf("published header is not a canonical v4 log: %v", err)
	}
}

func TestFileStoreDefersRecoverableCompleteTailRepairUntilAppend(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/recoverable-tail"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("recoverable-tail")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	start := Event{Type: "turn/start", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{"turn":1}`)}
	if err := journal.Append(ctx, []Event{start}); err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, header.ID)
	if err != nil || len(artifact.Events) != 1 {
		t.Fatalf("recoverable suffix open = %+v, %v", artifact, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(before) {
		t.Fatalf("read-only open repaired the file prematurely: %v", err)
	}
	reopened, _, err = store.Open(ctx, header.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Append(ctx, nil); err != nil {
		t.Fatal(err)
	}
	unchanged, err = os.ReadFile(path)
	if err != nil || string(unchanged) != string(before) {
		t.Fatalf("empty append repaired the file prematurely: %v", err)
	}
	end := Event{Type: "turn/end", Seq: 1, Time: header.CreatedAt + 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}
	if err := reopened.Append(ctx, []Event{end}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(repaired), "{not json") {
		t.Fatalf("first append did not repair suffix: %v, %s", err, repaired)
	}
	inspection, err := InspectV4JSONL(strings.NewReader(string(repaired)))
	if err != nil || len(inspection.Events) != 2 {
		t.Fatalf("repaired log = %+v, %v", inspection, err)
	}
}

func TestFileStoreRejectsDamageBeforeCommittedTurnEnd(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/committed-damage"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("committed-damage")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	start := Event{Type: "turn/start", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{"turn":1}`)}
	if err := journal.Append(ctx, []Event{start}); err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	end, err := EncodeEvent(Event{Type: "turn/end", Seq: 2, Time: header.CreatedAt + 3, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append([]byte("{not json\n"), append(end, '\n')...)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if journal, _, err := store.Open(ctx, header.ID); err == nil {
		_ = journal.Close()
		t.Fatal("opened damaged data before a committed turn/end")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rejected committed damage changed the file: %v", err)
	}
}

func TestFileStoreRecoversUncommittedSequenceGap(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/seq-gap"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("seq-gap")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	start := Event{Type: "turn/start", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{"turn":1}`)}
	if err := journal.Append(ctx, []Event{start}); err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	gap, err := EncodeEvent(Event{Type: "step/start", Seq: 2, Time: header.CreatedAt + 2, Data: json.RawMessage(`{"turn":1,"step":1}`)}, 2)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(gap, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, header.ID)
	if err != nil || len(artifact.Events) != 1 {
		t.Fatalf("open uncommitted seq gap = %+v, %v", artifact, err)
	}
	end := Event{Type: "turn/end", Seq: 1, Time: header.CreatedAt + 3, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}
	if err := reopened.Append(ctx, []Event{end}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectV4JSONL(bytes.NewReader(repaired))
	if err != nil || len(inspection.Events) != 2 || bytes.Contains(repaired, gap) {
		t.Fatalf("sequence-gap repair = %+v, %v", inspection, err)
	}
}

func TestFileStoreRejectsUnknownRequiredAndMalformedNativeTail(t *testing.T) {
	for _, candidate := range []Event{
		{Type: "future/required", Seq: 0, Time: 1, Data: json.RawMessage(`{}`)},
		{Type: "tool/result", Seq: 0, Time: 1, Data: json.RawMessage(`{}`), SurfaceOp: json.RawMessage(`"append"`)},
	} {
		t.Run(candidate.Type, func(t *testing.T) {
			ctx := context.Background()
			cwd := "/root/harness/workspaces/invalid-tail"
			store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
			header := v4TestHeader("invalid-tail")
			header.CWD = cwd
			journal, err := store.Create(ctx, header)
			if err != nil {
				t.Fatal(err)
			}
			path := journal.Path()
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			line, err := EncodeEvent(candidate, 0)
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(append(line, '\n')); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, _, err := store.Open(ctx, header.ID); err == nil {
				_ = reopened.Close()
				t.Fatal("opened a structurally unsupported committed row")
			}
		})
	}
}

func TestFileStoreRecoversIncompleteOrdinaryMessageTail(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/incomplete-message"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("incomplete-message")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	start := Event{Type: "turn/start", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{"turn":1}`)}
	if err := journal.Append(ctx, []Event{start}); err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	malformed, err := EncodeEvent(Event{Type: "assistant/message", Seq: 1, Time: header.CreatedAt + 2, Data: json.RawMessage(`{}`), SurfaceOp: json.RawMessage(`"append"`)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(malformed, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, artifact, err := store.Open(ctx, header.ID)
	if err != nil || len(artifact.Events) != 1 {
		t.Fatalf("open incomplete ordinary-message suffix = %+v, %v", artifact, err)
	}
	end := Event{Type: "turn/end", Seq: 1, Time: header.CreatedAt + 3, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)}
	if err := reopened.Append(ctx, []Event{end}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil || bytes.Contains(repaired, malformed) {
		t.Fatalf("incomplete ordinary-message suffix survived repair: %v", err)
	}
}

func TestFileStoreRejectsIncompleteOrdinaryMessageBeforeCommittedEnd(t *testing.T) {
	header := v4TestHeader("incomplete-before-end")
	header.CWD = "/root/harness/workspaces/incomplete-before-end"
	first, err := EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	rows := []Event{
		{Type: "turn/start", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: "assistant/message", Seq: 1, Time: header.CreatedAt + 2, Data: json.RawMessage(`{}`), SurfaceOp: json.RawMessage(`"append"`)},
		{Type: "turn/end", Seq: 2, Time: header.CreatedAt + 3, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	log := append(first, '\n')
	for index, row := range rows {
		line, err := EncodeEvent(row, uint64(index))
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, append(line, '\n')...)
	}
	if _, _, err := inspectRecoverableFileLog(log); err == nil || !strings.Contains(err.Error(), "message event requires message object") {
		t.Fatalf("committed turn/end hid incomplete ordinary message: %v", err)
	}
}

func TestFileStoreRejectsRetiredHeaderAfterCorruptRowEvenWithMissingEnvelope(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/retired-header"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("retired-header")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{not json\n" + `{"type":"request/header","data":{"header":{"system":"retired"}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, _, err := store.Open(ctx, header.ID); err == nil || !strings.Contains(err.Error(), "retired header.system") {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("missing seq hid retired header syntax: %v", err)
	}
}

func TestFileStoreRejectsPluginSystemSourceAfterCorruptRow(t *testing.T) {
	ctx := context.Background()
	cwd := "/root/harness/workspaces/plugin-system"
	store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: cwd, Claim: v4Claim}
	header := v4TestHeader("plugin-system")
	header.CWD = cwd
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := `{"type":"system/message","data":{"turn":1,"step":1,"message":{"id":"x","role":"system","content":[],"source":{"kind":"plugin"}}}}`
	if _, err := file.WriteString("{not json\n" + row + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, _, err := store.Open(ctx, header.ID); err == nil || !strings.Contains(err.Error(), "retired plugin source") {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("corrupt prefix hid retired system source: %v", err)
	}
}

func TestFileStoreRejectsUnknownRequiredAfterCorruptRow(t *testing.T) {
	header := v4TestHeader("unknown-after-corruption")
	header.CWD = "/root/harness/workspaces/unknown-after-corruption"
	first, err := EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := EncodeEvent(Event{Type: "future/required", Seq: 0, Time: header.CreatedAt + 1, Data: json.RawMessage(`{}`)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	log := append(append(append(first, '\n'), []byte("{not json\n")...), append(unknown, '\n')...)
	if _, _, err := inspectRecoverableFileLog(log); err == nil || !strings.Contains(err.Error(), "unknown required") {
		t.Fatalf("corrupt prefix hid unknown required event: %v", err)
	}
}
