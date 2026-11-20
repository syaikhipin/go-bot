package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, watch bool) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), Filename), Options{Watch: watch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func grant(value string) *Record { return &Record{Kind: "grant", Payload: json.RawMessage(value)} }

func TestReferencesAndRecordsKeepUpstreamLayeringAndPresence(t *testing.T) {
	s := openTestStore(t, false)
	s.SetProvided(map[string]string{"MAIL_KEY": "provided", "EMPTY": ""})
	if value, err := s.Resolve("MAIL_KEY"); err != nil || value.Value != "provided" || value.Source != "controller" {
		t.Fatalf("fallback: %+v %v", value, err)
	}
	if value, _ := s.Resolve("EMPTY"); value != nil {
		t.Fatal("blank supplied credential is present")
	}
	if err := s.Set(t.Context(), "MAIL_KEY", "managed"); err != nil {
		t.Fatal(err)
	}
	if value, _ := s.Resolve("MAIL_KEY"); value.Value != "managed" || value.Source != "file" {
		t.Fatal(value)
	}
	if info, _ := s.Describe("MAIL_KEY"); !info.Writable || !info.Configured || strings.Contains(info.Masked, "managed") {
		t.Fatalf("unsafe descriptor: %+v", info)
	}
	if err := s.Set(t.Context(), "MAIL_KEY", ""); err == nil {
		t.Fatal("empty write succeeded")
	}
	if err := s.Unset(t.Context(), "MAIL_KEY"); err != nil {
		t.Fatal(err)
	}
	if value, _ := s.Resolve("MAIL_KEY"); value.Value != "provided" {
		t.Fatal(value)
	}
	if _, err := s.ModifyRecord(t.Context(), "mail/personal", func(*Record) (*Record, error) { return &Record{Kind: "api-key"}, nil }); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.DescribeRecord("mail/personal"); !info.Configured || info.Kind != "api-key" {
		t.Fatalf("empty api-key record must remain present: %+v", info)
	}
	if _, err := s.ModifyRecord(t.Context(), "mail/personal", func(*Record) (*Record, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.ListRecords(); len(entries) != 1 || entries[0].Key != "mail/personal" {
		t.Fatal(entries)
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v %v", info, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if info, _ := next.DescribeRecord("mail/personal"); !info.Configured {
		t.Fatal("record did not survive restart")
	}
	if value, _ := next.Resolve("MAIL_KEY"); value != nil {
		t.Fatal("Controller fallback leaked into durable document")
	}
}

func TestSerializedMutationReadsLatestRecordAndNeverLeaksMutableSnapshots(t *testing.T) {
	s := openTestStore(t, false)
	var wg sync.WaitGroup
	failures := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ModifyRecord(t.Context(), "calendar/account", func(current *Record) (*Record, error) {
				n := 0
				if current != nil {
					if err := json.Unmarshal(current.Payload, &n); err != nil {
						return nil, err
					}
				}
				data, _ := json.Marshal(n + 1)
				return &Record{Kind: "grant", Payload: data}, nil
			})
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.ReadRecord("calendar/account")
	if err != nil || string(result.Payload) != "24" {
		t.Fatalf("lost update: %+v %v", result, err)
	}
	returned, err := s.ModifyRecord(t.Context(), "calendar/account", func(current *Record) (*Record, error) { return current, nil })
	if err != nil {
		t.Fatal(err)
	}
	returned.Payload[0] = '0'
	if stored, _ := s.ReadRecord("calendar/account"); string(stored.Payload) != "24" {
		t.Fatal("mutation result leaked authoritative memory")
	}
	result.Payload[0] = '0'
	result, _ = s.ReadRecord("calendar/account")
	if string(result.Payload) != "24" {
		t.Fatal("read mutated authoritative record")
	}
	_, err = s.ModifyRecord(t.Context(), "calendar/account", func(current *Record) (*Record, error) {
		current.Payload[0] = '0'
		return nil, errors.New("provider rejected")
	})
	if err == nil {
		t.Fatal("callback failure disappeared")
	}
	result, _ = s.ReadRecord("calendar/account")
	if string(result.Payload) != "24" {
		t.Fatal("failed callback changed record")
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = s.ModifyRecord(ctx, "calendar/account", func(*Record) (*Record, error) { cancel(); return grant(`99`), nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled mutation=%v", err)
	}
	result, _ = s.ReadRecord("calendar/account")
	if string(result.Payload) != "24" {
		t.Fatal("cancelled callback committed")
	}
}

func TestRecordCallbacksCanReadCommittedStateAndCloseDrains(t *testing.T) {
	s := openTestStore(t, false)
	admitted := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.ModifyRecord(context.Background(), "mail/primary", func(*Record) (*Record, error) {
			close(admitted)
			if _, err := s.ListRecords(); err != nil {
				return nil, err
			}
			<-release
			return grant(`{"refresh":"private"}`), nil
		})
		done <- err
	}()
	<-admitted
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close did not drain admitted mutation")
	case <-time.After(15 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-closed
	if _, err := s.Resolve("MAIL_KEY"); err == nil {
		t.Fatal("closed store still accepts reads")
	}
	next, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if record, _ := next.ReadRecord("mail/primary"); record == nil {
		t.Fatal("admitted commit lost during close")
	}
}

func TestPrivateDocumentEditsAndObserversFollowCommittedFacts(t *testing.T) {
	s := openTestStore(t, true)
	updates := make(chan Update, 20)
	s.Subscribe(func(Update) { panic("observer failure") })
	s.Subscribe(func(u Update) { updates <- u })
	if _, err := s.ModifyRecord(t.Context(), "mail/primary", func(*Record) (*Record, error) { return grant(`{"token":"secret","nested":[1,null,true]}`), nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-updates:
		if len(u.Records) != 1 || u.Records[0] != "mail/primary" {
			t.Fatal(u)
		}
	case <-time.After(time.Second):
		t.Fatal("committed update missing")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte("# user documentation\n"), data...)
	data = []byte(strings.Replace(string(data), "refs: {}", "refs:\n  MAIL_KEY: from-editor # keep this note", 1))
	if err = os.WriteFile(s.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case u := <-updates:
			if len(u.References) > 0 {
				goto observed
			}
		case <-deadline:
			t.Fatal("external edit was not observed")
		}
	}
observed:
	if value, _ := s.Resolve("MAIL_KEY"); value == nil || value.Value != "from-editor" {
		t.Fatal(value)
	}
	if err = s.Set(t.Context(), "OTHER_KEY", "another"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(s.path)
	if !strings.Contains(string(data), "# user documentation") || !strings.Contains(string(data), "# keep this note") {
		t.Fatal("unrelated comments were lost")
	}
	if err = s.DeleteRecord(t.Context(), "mail/primary"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteRecord(t.Context(), "mail/primary"); err != nil {
		t.Fatal("absent delete must be a no-op", err)
	}
}

func TestInvalidRecordNeverPoisonsNextBoot(t *testing.T) {
	s := openTestStore(t, false)
	for _, record := range []*Record{{Kind: "other"}, {Kind: "grant", Payload: json.RawMessage(`{`)}, {Kind: "api-key", Key: new(string)}, {Kind: "api-key", Env: map[string]string{"bad-name": "value"}}} {
		if _, err := s.ModifyRecord(t.Context(), "mail/primary", func(*Record) (*Record, error) { return record, nil }); err == nil {
			t.Fatalf("invalid record stored: %+v", record)
		}
	}
	if record, _ := s.ReadRecord("mail/primary"); record != nil {
		t.Fatal("failed record was published")
	}
	if _, err := s.ModifyRecord(t.Context(), "mail/primary", func(*Record) (*Record, error) { return grant(`null`), nil }); err != nil {
		t.Fatal("JSON null grant payload must be valid", err)
	}
	for _, text := range []string{"refs:\n  X: true\n", "refs:\n  X: a\n  X: b\n", "refs: {}\n---\nrefs: {}\n", "records:\n  bad: {kind: api-key}\n"} {
		path := filepath.Join(t.TempDir(), Filename)
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if next, err := Open(path, Options{}); err == nil {
			next.Close()
			t.Fatalf("invalid document accepted: %s", text)
		}
	}
}

func TestGrantObjectOrderingDoesNotEmitPhantomReloadChanges(t *testing.T) {
	s := openTestStore(t, true)
	events := make(chan Update, 8)
	s.Subscribe(func(u Update) { events <- u })
	_, err := s.ModifyRecord(t.Context(), "mail/primary", func(*Record) (*Record, error) { return grant(`{"z":1,"a":{"second":2,"first":1}}`), nil })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("write notification missing")
	}
	select {
	case extra := <-events:
		t.Fatalf("YAML/JSON ordering emitted an uncommitted change: %+v", extra)
	case <-time.After(250 * time.Millisecond):
	}
}
