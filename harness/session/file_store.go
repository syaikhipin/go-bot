package sessionformat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go-bot/internal/durablefs"
)

// FileStore is the v4 persistence backend for either user cloud-computer
// mode. Root is /root/harness/.dsh/sessions in the container; a NAS-mapped
// Host uses the same native file operations through its mounted directory.
type FileStore struct {
	Root          string
	ProjectCWD    string
	RootSessionID string // Optional built-in coordinator ancestor; not a metadata classification.
	Claim         OwnerClaim
}

func (s FileStore) Path(sessionID string) (string, error) {
	if !filepath.IsAbs(s.Root) || !v4KeyComponent.MatchString(sessionID) ||
		(s.RootSessionID != "" && (!v4KeyComponent.MatchString(s.RootSessionID) || s.RootSessionID == sessionID)) {
		return "", errors.New("invalid local Session v4 path")
	}
	project, err := ProjectKey(s.ProjectCWD)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Root, project, sessionID, Filename), nil
}

func (s FileStore) validateHeader(header Header) error {
	if err := header.Validate(); err != nil {
		return err
	}
	return validateSessionProject(s.ProjectCWD, header)
}

// ReadHeader inspects only the canonical first line. History listing must
// not claim the Session writer or load its entire event log into memory.
func (s FileStore) ReadHeader(ctx context.Context, sessionID string) (Header, error) {
	filename, err := s.Path(sessionID)
	if err != nil {
		return Header{}, err
	}
	header, err := readStoredHeader(ctx, filename, sessionID)
	if err != nil {
		return Header{}, err
	}
	return header, s.validateHeader(header)
}

// ReadProjectHeader resolves a Session's recorded cwd when the caller knows
// the journal's project key but has no workspace binding for this Session.
func ReadProjectHeader(ctx context.Context, root, project, sessionID string) (Header, error) {
	if !filepath.IsAbs(root) || filepath.Base(project) != project || project == "." || project == ".." ||
		!v4KeyComponent.MatchString(sessionID) {
		return Header{}, errors.New("invalid Session project location")
	}
	header, err := readStoredHeader(ctx, filepath.Join(root, project, sessionID, Filename), sessionID)
	if err != nil {
		return Header{}, err
	}
	key, err := ProjectKey(header.CWD)
	if err != nil || key != project {
		return Header{}, errors.New("local Session cwd differs from its project key")
	}
	return header, header.Validate()
}

func readStoredHeader(ctx context.Context, filename, sessionID string) (Header, error) {
	if err := ctx.Err(); err != nil {
		return Header{}, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return Header{}, err
	}
	defer file.Close()
	line, err := bufio.NewReader(io.LimitReader(file, maxV4HeaderLineBytes+1)).ReadBytes('\n')
	if err != nil || len(line) > maxV4HeaderLineBytes {
		return Header{}, errors.New("invalid local Session v4 header line")
	}
	header, err := DecodeHeader(line[:len(line)-1])
	if err != nil {
		return Header{}, err
	}
	if header.ID != sessionID {
		return Header{}, errors.New("local Session v4 header ID differs from its path")
	}
	return header, nil
}

func (s FileStore) claim(ctx context.Context, sessionID string) (func(), error) {
	if s.Claim == nil {
		return nil, errors.New("local Session v4 writer requires an exclusive claim")
	}
	release, err := s.Claim(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, errors.New("local Session v4 owner claim returned no release function")
	}
	return release, nil
}

func (s FileStore) Create(ctx context.Context, header Header) (*FileJournal, error) {
	return s.create(ctx, header, nil)
}

// CreatePrepared atomically publishes the header with the complete validated
// constructor seed. Readers can never observe a seeded header without its cut.
// This backend copies the entire prefix; it does not borrow a live parent file.
func (s FileStore) CreatePrepared(ctx context.Context, header Header, events []Event) (*FileJournal, error) {
	var cut uint64
	if header.IsSeeded {
		for _, event := range events {
			if event.Type == "session/end-seed" {
				var marker struct {
					Inherited bool `json:"inherited"`
				}
				if json.Unmarshal(event.Data, &marker) == nil && marker.Inherited {
					cut = event.Seq
				}
			}
		}
	}
	if _, err := replaySession(PhysicalArtifact{Header: header, Events: events, InheritedEventCount: cut}, nil, 0); err != nil {
		return nil, err
	}
	return s.create(ctx, header, events)
}

func (s FileStore) create(ctx context.Context, header Header, events []Event) (*FileJournal, error) {
	if err := s.validateHeader(header); err != nil {
		return nil, err
	}
	filename, err := s.Path(header.ID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	release, err := s.claim(ctx, header.ID)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	if err := durablefs.EnsureDirectory(filepath.Dir(filename)); err != nil {
		return nil, err
	}
	line, err := EncodeHeader(header)
	if err != nil {
		return nil, err
	}
	// Publish only a fully written, synced header. A failed or interrupted
	// creation must not leave an empty canonical Session log that cannot be
	// activated or resumed. link() also refuses to replace an existing log.
	file, err := os.CreateTemp(filepath.Dir(filename), ".session-create-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		if !owned {
			_ = file.Close()
		}
		// The canonical hard link may already exist when directory fsync fails.
		// Always remove the staging name; leaving it behind on an uncertain NAS
		// publication would accumulate orphaned Session files.
		_ = os.Remove(file.Name())
	}()
	if err := file.Chmod(0600); err != nil {
		return nil, err
	}
	var body bytes.Buffer
	body.Write(line)
	body.WriteByte('\n')
	for index, event := range events {
		row, err := EncodeEvent(event, uint64(index))
		if err != nil {
			return nil, err
		}
		body.Write(row)
		body.WriteByte('\n')
	}
	data := body.Bytes()
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return nil, fmt.Errorf("stage local Session v4 header: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync local Session v4 header: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Link(file.Name(), filename); err != nil {
		return nil, fmt.Errorf("publish local Session v4 header: %w: %w", err, ErrV4UncertainWrite)
	}
	if err := durablefs.SyncDirectory(filepath.Dir(filename)); err != nil {
		return nil, fmt.Errorf("sync published local Session v4 header: %w: %w", err, ErrV4UncertainWrite)
	}
	// The canonical link is durable. Removing its redundant temporary link is
	// best-effort and must not turn a successful Session creation into failure.
	owned = true
	return &FileJournal{file: file, filename: filename, release: release, nextSeq: uint64(len(events)), repairAt: -1}, nil
}

func (s FileStore) Open(ctx context.Context, sessionID string) (*FileJournal, PhysicalArtifact, error) {
	filename, err := s.Path(sessionID)
	if err != nil {
		return nil, PhysicalArtifact{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, PhysicalArtifact{}, err
	}
	release, err := s.claim(ctx, sessionID)
	if err != nil {
		return nil, PhysicalArtifact{}, err
	}
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	file, err := os.OpenFile(filename, os.O_RDWR, 0)
	if err != nil {
		return nil, PhysicalArtifact{}, err
	}
	defer func() {
		if !owned {
			_ = file.Close()
		}
	}()
	artifact, repairAt, err := s.readArtifact(file, sessionID)
	if err != nil {
		return nil, PhysicalArtifact{}, err
	}
	if _, err := file.Seek(0, 2); err != nil {
		return nil, PhysicalArtifact{}, err
	}
	owned = true
	return &FileJournal{file: file, filename: filename, nextSeq: uint64(len(artifact.Events)), release: release, repairAt: int64(repairAt)}, artifact, nil
}

// ReadArtifact inspects the stored event prefix without claiming the writer,
// repairing the journal, or starting a Session.
func (s FileStore) ReadArtifact(ctx context.Context, sessionID string) (PhysicalArtifact, error) {
	filename, err := s.Path(sessionID)
	if err != nil {
		return PhysicalArtifact{}, err
	}
	if err := ctx.Err(); err != nil {
		return PhysicalArtifact{}, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return PhysicalArtifact{}, err
	}
	defer file.Close()
	artifact, _, err := s.readArtifact(file, sessionID)
	return artifact, err
}

func (s FileStore) readArtifact(file *os.File, sessionID string) (PhysicalArtifact, int, error) {
	raw, err := io.ReadAll(file)
	if err != nil {
		return PhysicalArtifact{}, -1, err
	}
	if len(raw) == 0 {
		return PhysicalArtifact{}, -1, errors.New("local Session v4 log is empty")
	}
	inspection, repairAt, err := inspectRecoverableFileLog(raw)
	if err != nil {
		return PhysicalArtifact{}, -1, err
	}
	artifact := inspection.PhysicalArtifact
	if artifact.Header.ID != sessionID {
		return PhysicalArtifact{}, -1, errors.New("local Session v4 header ID differs from its path")
	}
	if err := s.validateHeader(artifact.Header); err != nil {
		return PhysicalArtifact{}, -1, err
	}
	return artifact, repairAt, nil
}

// DeepSeek's recoverable JSONL scan preserves the contiguous event prefix and
// refuses to discard damage before a later committed turn/end. A torn physical
// tail is only repaired when the write handle next appends; opening a Session
// must not itself change the file.
func inspectRecoverableFileLog(raw []byte) (Inspection, int, error) {
	headerEnd := bytes.IndexByte(raw, '\n')
	if headerEnd < 0 {
		return Inspection{}, -1, errors.New("local Session v4 header is incomplete")
	}
	committedEnd := headerEnd + 1
	var issue error
	seq := uint64(0)
	row := 1
	for offset := committedEnd; offset < len(raw); row++ {
		relativeEnd := bytes.IndexByte(raw[offset:], '\n')
		if relativeEnd < 0 {
			break // An unterminated final fragment is never a committed row.
		}
		end := offset + relativeEnd + 1
		line := bytes.TrimSpace(raw[offset : end-1])
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
			if issue == nil {
				issue = fmt.Errorf("Session v4 row %d: invalid committed JSON", row)
			}
			offset = end
			continue
		}
		var eventType string
		_ = json.Unmarshal(fields["type"], &eventType)
		if err := validateV4RawAdmission(fields, eventType); err != nil {
			return Inspection{}, -1, fmt.Errorf("Session v4 row %d: %w", row, err)
		}
		if eventType == "turn/end" && issue != nil {
			return Inspection{}, -1, issue
		}
		if issue != nil {
			offset = end
			continue
		}
		ownSeq, seqErr := safeCount(fields["seq"])
		if seqErr != nil {
			if issue == nil {
				issue = fmt.Errorf("Session v4 row %d: %w", row, seqErr)
			}
			if eventType == "turn/end" {
				return Inspection{}, -1, issue
			}
			offset = end
			continue
		}
		event, err := DecodeEvent(line, ownSeq)
		if err != nil {
			if issue == nil {
				issue = fmt.Errorf("Session v4 row %d: %w", row, err)
			}
			if eventType == "turn/end" {
				return Inspection{}, -1, issue
			}
			offset = end
			continue
		}
		if !knownV4EventTypes[event.Type] && !event.Ignorable {
			return Inspection{}, -1, fmt.Errorf("unknown required Session v4 event %q", event.Type)
		}
		if err := ValidateV4Rows([]Event{event}); err != nil {
			issue = fmt.Errorf("Session v4 row %d: %w", row, err)
			if event.Type == "turn/end" {
				return Inspection{}, -1, issue
			}
			offset = end
			continue
		}
		if event.Seq != seq {
			issue = fmt.Errorf("Session v4 row %d: seq gap: got %d, want %d", row, event.Seq, seq)
			if event.Type == "turn/end" {
				return Inspection{}, -1, issue
			}
			offset = end
			continue
		}
		seq++
		committedEnd = end
		offset = end
	}
	inspection, err := InspectV4JSONL(bytes.NewReader(raw[:committedEnd]))
	if err != nil {
		return Inspection{}, -1, err
	}
	if committedEnd != len(raw) {
		return inspection, committedEnd, nil
	}
	return inspection, -1, nil
}

// The upstream scanner applies native V4 admission to every JSON object,
// including rows after a recoverable corruption boundary, before asking the
// physical decoder whether that row is complete. Keep the hard refusals here
// narrower than full event validation: an incomplete ordinary message at the
// end of an interrupted turn may be truncated, but retired syntax, tool-role
// corruption and producer-attribution violations may not be hidden that way.
func validateV4RawAdmission(fields map[string]json.RawMessage, eventType string) error {
	if eventType == "tool/code-dispatch" || eventType == "tool/code-dispatch-start" {
		if !bytes.Equal(fields["ignorable"], []byte("true")) {
			return fmt.Errorf("retired Session v4 event %q", eventType)
		}
	}
	if eventType != "" && !knownV4EventTypes[eventType] && !bytes.Equal(fields["ignorable"], []byte("true")) {
		return fmt.Errorf("unknown required Session v4 event %q", eventType)
	}
	data, dataErr := objectValue(fields["data"])
	if eventType == "request/header" {
		if dataErr != nil {
			return errors.New("request/header requires object data")
		}
		header, err := objectValue(data["header"])
		if err != nil {
			return errors.New("request/header requires header object")
		}
		if _, retired := header["system"]; retired {
			return errors.New("request/header uses retired header.system")
		}
		var definitions []json.RawMessage
		if json.Unmarshal(header["tools"], &definitions) == nil {
			for _, raw := range definitions {
				definition, err := objectValue(raw)
				if err == nil {
					if deferred, present := definition["deferLoading"]; present && !bytes.Equal(deferred, []byte("true")) {
						return errors.New("tool deferLoading must be true when present")
					}
				}
			}
		}
	}
	if eventType == "system/message" {
		if dataErr != nil {
			return errors.New("system/message requires object data")
		}
		var constructor Event
		constructor.Type, constructor.Data, constructor.SurfaceOp = eventType, fields["data"], fields["surfaceOp"]
		constructor.Seq, _ = safeCount(fields["seq"])
		if err := validateSystemMessage(data, isInitialSystemMessage(constructor)); err != nil {
			return err
		}
	}
	if eventType == "tool/result" {
		if dataErr != nil {
			return errors.New("tool/result requires object data")
		}
		event := Event{Type: eventType, Data: fields["data"], SurfaceOp: fields["surfaceOp"], SourceEventSeqs: fields["sourceEventSeqs"]}
		if seq, err := safeCount(fields["seq"]); err == nil {
			event.Seq = seq
		}
		if err := validateToolResult(event, data); err != nil {
			return err
		}
	}
	if eventType == "developer/message" {
		if dataErr != nil {
			return errors.New("developer/message requires object data")
		}
		if err := validateDeveloperMessage(data); err != nil {
			return err
		}
	}
	if dataErr != nil {
		return nil
	}
	event := Event{Type: eventType, Data: fields["data"]}
	if err := validateRetiredContent(event, data); err != nil {
		return err
	}
	if eventType == "assistant/message" || eventType == "assistant/attempt" {
		if err := validateStreamSyntax(data); err != nil {
			return err
		}
	}
	var messages []json.RawMessage
	switch eventType {
	case "user/message":
		messages = append(messages, fields["data"])
	case "developer/message", "system/message", "assistant/message", "tool/result":
		messages = append(messages, data["message"])
	case "agent/inbox/spliced", "session/title-llm-request":
		field := "inserted"
		if eventType == "session/title-llm-request" {
			field = "messages"
		}
		_ = json.Unmarshal(data[field], &messages)
	}
	for _, raw := range messages {
		message, err := objectValue(raw)
		if err != nil {
			continue
		}
		if eventType != "developer/message" && bytes.Equal(message["role"], []byte(`"developer"`)) {
			return errors.New("developer role requires developer/message")
		}
		if source, err := objectValue(message["source"]); err == nil && bytes.Equal(source["kind"], []byte(`"plugin"`)) {
			return errors.New("v4 message rejects retired plugin source")
		}
		if err := validateToolChanges(message["content"], eventType == "developer/message"); err != nil {
			return err
		}
	}
	return nil
}

// FileJournal serializes and fsyncs each committed event batch. A confirmed
// rollback leaves the same suffix retryable; an uncertain write poisons the
// handle, and reopening repairs only a recoverable final tail.
type FileJournal struct {
	mu       sync.Mutex
	file     *os.File
	filename string
	nextSeq  uint64
	release  func()
	// Nil uses the normal file writer; tests inject a one-shot I/O failure.
	appendBatch func(*os.File, []byte) error
	poison      bool
	closed      bool
	repairAt    int64
}

func (j *FileJournal) Append(ctx context.Context, events []Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.poison {
		return ErrV4UncertainWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	var batch bytes.Buffer
	for index, event := range events {
		if !knownV4EventTypes[event.Type] && !event.Ignorable {
			return fmt.Errorf("unknown required Session v4 event %q", event.Type)
		}
		line, err := EncodeEvent(event, j.nextSeq+uint64(index))
		if err != nil {
			return err
		}
		batch.Write(line)
		batch.WriteByte('\n')
	}
	if err := ValidateV4Rows(events); err != nil {
		return err
	}
	if j.repairAt >= 0 {
		if err := j.file.Truncate(j.repairAt); err != nil {
			j.poison = true
			return fmt.Errorf("truncate torn local Session v4 tail: %w: %w", err, ErrV4UncertainWrite)
		}
		if err := j.file.Sync(); err != nil {
			j.poison = true
			return fmt.Errorf("sync repaired local Session v4 tail: %w: %w", err, ErrV4UncertainWrite)
		}
		if _, err := j.file.Seek(j.repairAt, io.SeekStart); err != nil {
			j.poison = true
			return fmt.Errorf("seek repaired local Session v4 tail: %w: %w", err, ErrV4UncertainWrite)
		}
		j.repairAt = -1
	}
	appendBatch := j.appendBatch
	if appendBatch == nil {
		appendBatch = func(file *os.File, data []byte) error { return appendSyncedFileBatch(file, data, nil, nil) }
	}
	if err := appendBatch(j.file, batch.Bytes()); err != nil {
		if errors.Is(err, ErrV4WriteRolledBack) && !errors.Is(err, ErrV4UncertainWrite) {
			return fmt.Errorf("append local Session v4 events: %w", err)
		}
		j.poison = true
		return fmt.Errorf("append local Session v4 events: %w: %w", err, ErrV4UncertainWrite)
	}
	j.nextSeq += uint64(len(events))
	return nil
}

// A failed write or fsync must not leave a complete-looking batch behind for
// the next owner to mistake for a committed checkpoint. Like DeepSeek's JSONL
// backend, roll back to the previous size before returning the error. The
// caller retries only after confirmed rollback; if rollback itself failed,
// only a fresh open can determine the physical outcome.
func appendSyncedFileBatch(file *os.File, data []byte, write func(*os.File, []byte) (int, error), syncFile func(*os.File) error) error {
	if write == nil {
		write = (*os.File).Write
	}
	if syncFile == nil {
		syncFile = (*os.File).Sync
	}
	before, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	written, err := write(file, data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = syncFile(file)
	}
	if err == nil {
		return nil
	}
	rollbackErr := file.Truncate(before)
	if rollbackErr == nil {
		rollbackErr = file.Sync()
	}
	_, seekErr := file.Seek(0, io.SeekEnd)
	if rollbackErr != nil || seekErr != nil {
		return errors.Join(err, fmt.Errorf("rollback local Session v4 append: %w", errors.Join(rollbackErr, seekErr)))
	}
	return errors.Join(err, ErrV4WriteRolledBack)
}

func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	err := j.file.Close()
	j.release()
	return err
}

func (j *FileJournal) Path() string { return j.filename }
