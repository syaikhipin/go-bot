package session

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestSessionHeaderRejectsLegacyGoMode(t *testing.T) {
	header := Header{
		SchemaVersion: SchemaVersion, SessionID: "session-1", UserID: "alice",
		Mode: "go", Workspace: "/workspace", CreatedAt: time.Now().UTC(),
	}
	if err := validateHeader(header); err == nil {
		t.Fatal("legacy Go execution mode was accepted")
	}
	header.Mode = "container"
	if err := validateHeader(header); err != nil {
		t.Fatalf("container mode was rejected: %v", err)
	}
}

func TestSessionHeaderDoesNotPersistAssemblyVersion(t *testing.T) {
	header := Header{
		SchemaVersion: SchemaVersion, SessionID: "session-1", UserID: "alice",
		Mode: "container", Workspace: "/workspace", CreatedAt: time.Now().UTC(),
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("assemblyVersion")) {
		t.Fatalf("session header persisted a deployment label: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("tier")) {
		t.Fatalf("session header persisted a retired image tier: %s", encoded)
	}
}
