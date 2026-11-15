package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type heldAppendJournal struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	batches [][]Event
}

func TestSessionCloseWaitsForInflightFlushThenDrainsRemainingTail(t *testing.T) {
	journal := &heldAppendJournal{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-journal.release:
		default:
			close(journal.release)
		}
	}()
	s, err := New(testHeader(), journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, "first"); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- s.Flush(context.Background()) }()
	select {
	case <-journal.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("journal append was not reached")
	}
	if _, err := s.Append("user/message", 1, 0, "second"); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Session close did not mark its write path closed")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := s.Append("user/message", 1, 0, "too late"); !errors.Is(err, ErrUncertainWrite) {
		t.Fatalf("append during close = %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("close returned before in-flight flush: %v", err)
	default:
	}
	close(journal.release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if got := len(s.DurableEvents()); got != 2 {
		t.Fatalf("close left %d durable events, want two", got)
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if len(journal.batches) != 2 || journal.batches[0][0].Seq != 1 || journal.batches[1][0].Seq != 2 {
		t.Fatalf("close did not drain consecutive batches: %#v", journal.batches)
	}
}

func (j *heldAppendJournal) Append(_ context.Context, events []Event) error {
	if len(events) > 0 && events[0].Seq == 1 {
		close(j.entered)
		<-j.release
	}
	j.mu.Lock()
	j.batches = append(j.batches, append([]Event(nil), events...))
	j.mu.Unlock()
	return nil
}

func (*heldAppendJournal) Flush(context.Context) error { return nil }
func (*heldAppendJournal) Close() error                { return nil }

func TestSlowJournalFlushDoesNotBlockSessionProjectionOrAppend(t *testing.T) {
	journal := &heldAppendJournal{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-journal.release:
		default:
			close(journal.release)
		}
	}()
	s, err := New(testHeader(), journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("user/message", 1, 0, "first"); err != nil {
		t.Fatal(err)
	}
	firstFlush := make(chan error, 1)
	go func() { firstFlush <- s.Flush(context.Background()) }()
	select {
	case <-journal.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("journal append was not reached")
	}
	type observation struct {
		durable int
		tail    int
		err     error
	}
	observed := make(chan observation, 1)
	go func() {
		durable := len(s.DurableEvents())
		_, err := s.Append("user/message", 1, 0, "second")
		tail, _, tailErr := s.EventTail(0)
		if err == nil {
			err = tailErr
		}
		observed <- observation{durable: durable, tail: len(tail), err: err}
	}()
	select {
	case result := <-observed:
		if result.err != nil || result.durable != 0 || result.tail != 2 {
			t.Fatalf("projection while journal is blocked = %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow journal blocked the in-memory Session")
	}
	close(journal.release)
	if err := <-firstFlush; err != nil {
		t.Fatal(err)
	}
	if got := len(s.DurableEvents()); got != 1 {
		t.Fatalf("first flush published %d events, want captured prefix of one", got)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(s.DurableEvents()); got != 2 {
		t.Fatalf("second flush published %d events, want two", got)
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if len(journal.batches) != 2 || len(journal.batches[0]) != 1 || journal.batches[0][0].Seq != 1 || len(journal.batches[1]) != 1 || journal.batches[1][0].Seq != 2 {
		t.Fatalf("journal batches are not consecutive captured prefixes: %#v", journal.batches)
	}
}
