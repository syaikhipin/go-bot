package session

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
	"regexp"
	"sync"
)

var safePathComponent = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Claim is supplied by the runtime owner coordinator. A process-local map is
// insufficient for production OSS mounts; an owner must be fenced before a
// second process can receive a write handle.
type Claim func(context.Context, string) (release func(), err error)

type FileStore struct {
	Root          string
	RootSessionID string // Set only for a child under the top-level Session.
	Claim         Claim
}

const JournalFilename = "session.v1.jsonl"

type storedLine struct {
	Kind    string          `json:"kind"`
	Header  *Header         `json:"header,omitempty"`
	Event   *Event          `json:"event,omitempty"`
	Fence   *journalFence   `json:"fence,omitempty"`
	Roll    *journalRoll    `json:"roll,omitempty"`
	Segment *journalSegment `json:"segment,omitempty"`
}

// A fence is a storage fact, not a model-facing Session event. An OSS writer
// appends it at an expected object position before accepting another turn.
type journalFence struct {
	EpochID string `json:"epochId"`
}

// Roll and segment markers are physical storage metadata. They never become
// model-facing events or consume a logical Event.Seq.
type journalRoll struct {
	NextIndex uint32 `json:"nextIndex"`
	RollID    string `json:"rollId"`
	FromSeq   uint64 `json:"fromSeq"`
}

type journalSegment struct {
	Index   uint32 `json:"index"`
	RollID  string `json:"rollId"`
	FromSeq uint64 `json:"fromSeq"`
}

type FileJournal struct {
	mu      sync.Mutex
	file    *os.File
	nextSeq uint64
	release func()
	closed  bool
	poison  bool
}

func (s FileStore) path(userID, sessionID string) (string, error) {
	path, err := s.journalPath(userID, sessionID)
	if err != nil {
		return "", err
	}
	if s.Claim == nil {
		return "", errors.New("cross-process claim is required")
	}
	return path, nil
}

func (s FileStore) journalPath(userID, sessionID string) (string, error) {
	directory, err := SessionDirectory(s.Root, userID, sessionID)
	if err != nil {
		return "", err
	}
	if s.RootSessionID != "" {
		if !safePathComponent.MatchString(s.RootSessionID) || s.RootSessionID == sessionID {
			return "", errors.New("invalid child Session root")
		}
		directory, err = SessionDirectory(s.Root, userID, s.RootSessionID)
		if err != nil {
			return "", err
		}
		directory = filepath.Join(directory, "subagents", sessionID)
	}
	return filepath.Join(directory, JournalFilename), nil
}

func (s FileStore) openExisting(userID, sessionID string, flag int) (*os.File, error) {
	path, err := s.journalPath(userID, sessionID)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, flag, 0)
}

// SessionDirectory is the canonical local/OSS-mount layout for one identity.
func SessionDirectory(root, userID, sessionID string) (string, error) {
	if !safePathComponent.MatchString(userID) || !safePathComponent.MatchString(sessionID) {
		return "", errors.New("unsafe user or session identifier")
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("absolute store root is required")
	}
	return filepath.Join(root, "users", userID, "sessions", sessionID), nil
}

func (s FileStore) Create(ctx context.Context, header Header) (*FileJournal, error) {
	if err := validateHeader(header); err != nil {
		return nil, err
	}
	if (s.RootSessionID == "") != (header.ParentSessionID == "") || (s.RootSessionID != "" && header.WorkspaceOwnerID != s.RootSessionID) {
		return nil, errors.New("Session journal root does not match header lineage")
	}
	path, err := s.path(header.UserID, header.SessionID)
	if err != nil {
		return nil, err
	}
	release, err := s.Claim(ctx, header.SessionID)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	encodedHeader, err := encodeStoredLine(storedLine{Kind: "header", Header: &header})
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	n, err := file.Write(encodedHeader)
	if err != nil || n != len(encodedHeader) {
		_ = file.Close()
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	owned = true
	return &FileJournal{file: file, nextSeq: 1, release: release}, nil
}

// Open accepts the physically valid contiguous prefix. A torn final JSONL
// line is removed before any new append; a malformed complete line is fatal.
func (s FileStore) Open(ctx context.Context, userID, sessionID string) (*FileJournal, Header, []Event, error) {
	if _, err := s.path(userID, sessionID); err != nil {
		return nil, Header{}, nil, err
	}
	release, err := s.Claim(ctx, sessionID)
	if err != nil {
		return nil, Header{}, nil, err
	}
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	file, err := s.openExisting(userID, sessionID, os.O_RDWR)
	if err != nil {
		return nil, Header{}, nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	header, events, err := readJournal(ctx, file, userID, sessionID, true)
	if err != nil {
		return nil, Header{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Header{}, nil, err
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, Header{}, nil, err
	}
	closeOnError = false
	owned = true
	return &FileJournal{file: file, nextSeq: uint64(len(events) + 1), release: release}, header, events, nil
}

// ReadSnapshot observes a stopped Session without acquiring a write claim or
// mutating a torn tail. Callers must route live Sessions to their owner and use
// its durable in-memory prefix instead of racing a writer on an OSS mount.
func (s FileStore) ReadSnapshot(userID, sessionID string) (Header, []Event, error) {
	file, err := s.openExisting(userID, sessionID, os.O_RDONLY)
	if err != nil {
		return Header{}, nil, err
	}
	defer file.Close()
	header, events, err := readJournal(context.Background(), file, userID, sessionID, false)
	if err != nil {
		return Header{}, nil, err
	}
	return header, events, nil
}

// ReadEventsAfter validates a stopped Session's complete log without keeping
// its complete event history in memory. Live Sessions must be read from their
// owner so a concurrent writer cannot make this projection inconsistent.
func (s FileStore) ReadEventsAfter(ctx context.Context, userID, sessionID string, after uint64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		return nil, errors.New("historical event page limit must be between 1 and 1000")
	}
	file, err := s.openExisting(userID, sessionID, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	_, events, _, err := readJournalWindow(&contextReader{ctx: ctx, reader: file}, userID, sessionID, nil, true, after, limit)
	return events, err
}

// ScanEvents validates a stopped Session while streaming detached events to
// one projection. It must not be used against a live writer on an OSS mount.
func (s FileStore) ScanEvents(ctx context.Context, userID, sessionID string, visit func(Event) error) error {
	if visit == nil {
		return errors.New("historical event visitor is required")
	}
	file, err := s.openExisting(userID, sessionID, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	_, _, _, err = readJournalVisit(&contextReader{ctx: ctx, reader: file}, userID, sessionID, nil, true, 0, 0, visit)
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// ReadHeader chooses the Assembly for a stopped Session without interpreting
// or repairing its event tail. The eventual write-open remains the authority
// for exclusive ownership and full log validation.
func (s FileStore) ReadHeader(userID, sessionID string) (Header, error) {
	file, err := s.openExisting(userID, sessionID, os.O_RDONLY)
	if err != nil {
		return Header{}, err
	}
	defer file.Close()
	return readStoredHeader(file, userID, sessionID)
}

const maxHeaderLineBytes = 64 << 10

// An event is also transported through the worker IPC with a 24 MiB ceiling.
// Allow framing overhead while bounding recovery memory on a corrupt log.
const maxStoredLineBytes = 32 << 20

func encodeStoredLine(row storedLine) ([]byte, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > maxStoredLineBytes {
		return nil, fmt.Errorf("Session journal line exceeds %d bytes", maxStoredLineBytes)
	}
	return append(encoded, '\n'), nil
}

func encodeEventBatch(events []Event, nextSeq uint64) ([]byte, error) {
	var batch []byte
	for index := range events {
		event := &events[index]
		if event.Seq != nextSeq+uint64(index) || event.Type == "" || event.At.IsZero() {
			return nil, errors.New("invalid or noncontiguous Session event batch")
		}
		line, err := encodeStoredLine(storedLine{Kind: "event", Event: event})
		if err != nil {
			return nil, err
		}
		batch = append(batch, line...)
	}
	return batch, nil
}

func readLineLimited(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > maxBytes {
			return nil, fmt.Errorf("Session journal line exceeds %d bytes", maxBytes)
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

var ErrNotSessionHeader = errors.New("object does not begin with a complete Session header")

func readStoredHeader(input io.Reader, userID, sessionID string) (Header, error) {
	line, err := bufio.NewReader(io.LimitReader(input, maxHeaderLineBytes+1)).ReadBytes('\n')
	if len(line) > maxHeaderLineBytes {
		return Header{}, errors.New("Session header exceeds 64 KiB")
	}
	if err != nil {
		return Header{}, fmt.Errorf("%w: %v", ErrNotSessionHeader, err)
	}
	var row storedLine
	if err := json.Unmarshal(bytes.TrimSuffix(line, []byte{'\n'}), &row); err != nil {
		return Header{}, fmt.Errorf("%w: %v", ErrNotSessionHeader, err)
	}
	if row.Kind != "header" || row.Header == nil || row.Event != nil || row.Fence != nil || row.Roll != nil || row.Segment != nil {
		return Header{}, ErrNotSessionHeader
	}
	if row.Header.UserID != userID || row.Header.SessionID != sessionID {
		return Header{}, errors.New("Session header does not match requested identity")
	}
	if err := validateHeader(*row.Header); err != nil {
		return Header{}, err
	}
	return *row.Header, nil
}

func readJournal(ctx context.Context, file *os.File, userID, sessionID string, repairTail bool) (Header, []Event, error) {
	var repair func(int64) error
	if repairTail {
		repair = func(validBytes int64) error {
			if err := file.Truncate(validBytes); err != nil {
				return fmt.Errorf("truncate torn JSONL tail: %w", err)
			}
			return file.Sync()
		}
	}
	header, events, _, err := readJournalFrom(&contextReader{ctx: ctx, reader: file}, userID, sessionID, repair, !repairTail)
	return header, events, err
}

func readJournalFrom(input io.Reader, userID, sessionID string, repairTail func(int64) error, ignoreTornTail bool) (Header, []Event, int64, error) {
	return readJournalWindow(input, userID, sessionID, repairTail, ignoreTornTail, 0, 0)
}

// readJournalWindow validates the entire physical log but retains only the
// requested event page. A zero limit is the full-log mode used for resume;
// positive limits bound memory for read-only historical event queries.
func readJournalWindow(input io.Reader, userID, sessionID string, repairTail func(int64) error, ignoreTornTail bool, after uint64, limit int) (Header, []Event, int64, error) {
	return readJournalVisit(input, userID, sessionID, repairTail, ignoreTornTail, after, limit, nil)
}

func readJournalVisit(input io.Reader, userID, sessionID string, repairTail func(int64) error, ignoreTornTail bool, after uint64, limit int, visit func(Event) error) (Header, []Event, int64, error) {
	reader := bufio.NewReader(input)
	var header Header
	var events []Event
	if limit > 0 {
		events = make([]Event, 0, min(limit, 200))
	}
	var validBytes int64
	var eventCount uint64
	var segmentIndex uint32
	var expectedSegment *journalRoll
	for {
		line, readErr := readLineLimited(reader, maxStoredLineBytes)
		if errors.Is(readErr, io.EOF) {
			if len(line) > 0 {
				if repairTail == nil && !ignoreTornTail {
					return Header{}, nil, 0, errors.New("incomplete final JSONL line")
				}
				if repairTail != nil {
					if err := repairTail(validBytes); err != nil {
						return Header{}, nil, 0, err
					}
				}
			}
			break
		}
		if readErr != nil {
			return Header{}, nil, 0, readErr
		}
		var row storedLine
		if err := json.Unmarshal(bytes.TrimSuffix(line, []byte{'\n'}), &row); err != nil {
			return Header{}, nil, 0, fmt.Errorf("invalid complete JSONL line at byte %d: %w", validBytes, err)
		}
		if validBytes == 0 {
			if row.Kind != "header" || row.Header == nil || row.Event != nil || row.Fence != nil || row.Roll != nil || row.Segment != nil || row.Header.UserID != userID || row.Header.SessionID != sessionID {
				return Header{}, nil, 0, errors.New("session header does not match requested owner")
			}
			if err := validateHeader(*row.Header); err != nil {
				return Header{}, nil, 0, err
			}
			header = *row.Header
		} else {
			switch row.Kind {
			case "fence":
				if expectedSegment != nil || row.Fence == nil || row.Fence.EpochID == "" || row.Header != nil || row.Event != nil || row.Roll != nil || row.Segment != nil {
					return Header{}, nil, 0, fmt.Errorf("invalid fence at byte %d", validBytes)
				}
			case "event":
				if expectedSegment != nil || row.Event == nil || row.Header != nil || row.Fence != nil || row.Roll != nil || row.Segment != nil || row.Event.Seq != eventCount+1 || row.Event.Type == "" || row.Event.At.IsZero() {
					return Header{}, nil, 0, fmt.Errorf("invalid event at byte %d", validBytes)
				}
				eventCount++
				if visit != nil {
					if err := visit(cloneEvent(*row.Event)); err != nil {
						return Header{}, nil, 0, err
					}
					break
				}
				if row.Event.Seq > after && (limit == 0 || len(events) < limit) {
					events = append(events, cloneEvent(*row.Event))
				}
			case "roll":
				if expectedSegment != nil || row.Roll == nil || row.Roll.NextIndex == 0 || row.Roll.NextIndex != segmentIndex+1 || row.Roll.RollID == "" || row.Roll.FromSeq != eventCount+1 || row.Header != nil || row.Event != nil || row.Fence != nil || row.Segment != nil {
					return Header{}, nil, 0, fmt.Errorf("invalid segment roll at byte %d", validBytes)
				}
				expectedSegment = row.Roll
			case "segment":
				if expectedSegment == nil || row.Segment == nil || row.Segment.Index != expectedSegment.NextIndex || row.Segment.RollID != expectedSegment.RollID || row.Segment.FromSeq != expectedSegment.FromSeq || row.Header != nil || row.Event != nil || row.Fence != nil || row.Roll != nil {
					return Header{}, nil, 0, fmt.Errorf("invalid segment continuation at byte %d", validBytes)
				}
				segmentIndex = row.Segment.Index
				expectedSegment = nil
			default:
				return Header{}, nil, 0, fmt.Errorf("invalid storage row at byte %d", validBytes)
			}
		}
		validBytes += int64(len(line))
	}
	if validBytes == 0 {
		return Header{}, nil, 0, errors.New("empty session journal")
	}
	if expectedSegment != nil {
		return Header{}, nil, 0, errors.New("Session journal segment continuation is missing")
	}
	return header, events, validBytes, nil
}

func (j *FileJournal) Append(ctx context.Context, events []Event) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.poison {
		return ErrUncertainWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := encodeEventBatch(events, j.nextSeq)
	if err != nil {
		return err
	}
	if len(encoded) == 0 {
		return nil
	}
	n, err := j.file.Write(encoded)
	if err != nil || n != len(encoded) {
		j.poison = true
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	j.nextSeq += uint64(len(events))
	return nil
}

func (j *FileJournal) Flush(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.poison {
		return ErrUncertainWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		j.poison = true
		return err
	}
	return nil
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

// ProcessClaims belongs to the legacy v1 Session storage and its tests. The
// formal v4 user Host has its own claim in sessionformat.
type ProcessClaims struct {
	mu    sync.Mutex
	owned map[string]bool
}

func (g *ProcessClaims) Acquire(_ context.Context, id string) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.owned == nil {
		g.owned = make(map[string]bool)
	}
	if g.owned[id] {
		return nil, fmt.Errorf("session %s already has a writer", id)
	}
	g.owned[id] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.owned, id)
			g.mu.Unlock()
		})
	}, nil
}
