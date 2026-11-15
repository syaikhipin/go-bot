package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileHistoricalEventsAfterRetainsOnlyPageAndValidatesTail(t *testing.T) {
	ctx := context.Background()
	store := FileStore{Root: t.TempDir(), Claim: (&ProcessClaims{}).Acquire}
	header := testHeader()
	journal, err := store.Create(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 60; index++ {
		if err := journal.Append(ctx, []Event{{Seq: uint64(index), Type: "user/message", At: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadEventsAfter(ctx, header.UserID, header.SessionID, 40, 5)
	if err != nil || len(page) != 5 || page[0].Seq != 41 || page[4].Seq != 45 {
		t.Fatalf("historical page = %#v, %v", page, err)
	}
	page, err = store.ReadEventsAfter(ctx, header.UserID, header.SessionID, 60, 5)
	if err != nil || page == nil || len(page) != 0 {
		t.Fatalf("cursor at end = %#v, %v", page, err)
	}
	path := filepath.Join(store.Root, "users", header.UserID, "sessions", header.SessionID, JournalFilename)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := fmt.Fprintln(file, `{"kind":"event","event":{"seq":62,"type":"user/message","at":"2026-01-01T00:00:00Z"}}`)
	if err := file.Close(); err != nil || writeErr != nil {
		t.Fatal(fmt.Errorf("append invalid tail: %w", writeErr))
	}
	if _, err := store.ReadEventsAfter(ctx, header.UserID, header.SessionID, 40, 5); err == nil {
		t.Fatal("cursor scan did not validate malformed complete tail after requested page")
	}
}
