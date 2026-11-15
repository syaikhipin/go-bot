package sessionformat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// This is the released-v4 snapshot at snapshots/sdk/serial-created/
// session.v4.jsonl in deepseek-ai/deepseek-harness commit
// d743267388641bc76f17c45ce8b4c231aed1d32c. The original corpus test
// supplies seq/time and resolves {{cwd}}/{{tools}} placeholders before asking
// its current reader to restore; do exactly that here, without rewriting the
// event payloads or surface operations.
func inspectOfficialSnapshot(t *testing.T, filename string) (Inspection, int) {
	t.Helper()
	file, err := os.Open("testdata/" + filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var physical bytes.Buffer
	scanner := bufio.NewScanner(file)
	row := 0
	for scanner.Scan() {
		var record map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("official row %d: %v", row, err)
		}
		if row == 0 {
			record["cwd"] = json.RawMessage(`"/workspace"`)
		} else {
			if string(record["type"]) == `"request/header"` {
				var data map[string]json.RawMessage
				if err := json.Unmarshal(record["data"], &data); err != nil {
					t.Fatal(err)
				}
				var header map[string]json.RawMessage
				if err := json.Unmarshal(data["header"], &header); err != nil {
					t.Fatal(err)
				}
				if string(header["tools"]) != `"{{tools}}"` {
					t.Fatalf("official request tools placeholder changed: %s", header["tools"])
				}
				delete(header, "tools")
				data["header"], _ = json.Marshal(header)
				record["data"], _ = json.Marshal(data)
			}
			record["seq"], _ = json.Marshal(row - 1)
			record["time"], _ = json.Marshal(row)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		physical.Write(encoded)
		physical.WriteByte('\n')
		row++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectV4JSONL(&physical)
	if err != nil {
		t.Fatalf("Go v4 reader rejected the official snapshot: %v", err)
	}
	return inspection, row - 1
}

func TestInspectOfficialDeepSeekV4SerialCreatedSnapshot(t *testing.T) {
	inspection, eventCount := inspectOfficialSnapshot(t, "deepseek-serial-created.v4.jsonl")
	if len(inspection.Events) != eventCount || len(inspection.Surface.Messages) != 5 || len(inspection.Inbox.NextTurn) != 0 || len(inspection.Inbox.NextStep) != 0 {
		t.Fatalf("official snapshot did not restore fully: events=%d surface=%d inbox=%+v",
			len(inspection.Events), len(inspection.Surface.Messages), inspection.Inbox)
	}
	if !strings.Contains(string(inspection.Surface.Messages[4]), "SDK snapshot OK") {
		t.Fatalf("official assistant message lost: %s", inspection.Surface.Messages[4])
	}
}

func TestInspectOfficialDeepSeekV4ReadImageSnapshot(t *testing.T) {
	inspection, eventCount := inspectOfficialSnapshot(t, "deepseek-read-image.v4.jsonl")
	if len(inspection.Events) != eventCount || len(inspection.Surface.Messages) < 5 {
		t.Fatalf("official image snapshot was truncated: events=%d surface=%d", len(inspection.Events), len(inspection.Surface.Messages))
	}
	var imageResults int
	for _, event := range inspection.Events {
		if event.Type == "tool/result" && bytes.Contains(event.Data, []byte(`"type":"image"`)) {
			imageResults++
		}
	}
	if imageResults == 0 {
		t.Fatal("official read_image attachment did not survive v4 restore")
	}
}

func TestInspectOfficialDeepSeekV4EmptyResponseRetrySnapshot(t *testing.T) {
	inspection, eventCount := inspectOfficialSnapshot(t, "deepseek-empty-retry.v4.jsonl")
	if len(inspection.Events) != eventCount {
		t.Fatalf("official retry snapshot truncated: %d of %d events", len(inspection.Events), eventCount)
	}
	var attempts, scheduled, started, assistants int
	for _, event := range inspection.Events {
		switch event.Type {
		case "assistant/attempt":
			attempts++
		case "llm/retry":
			scheduled++
		case "llm/retry-started":
			started++
		case "assistant/message":
			assistants++
		}
	}
	if attempts != 1 || scheduled != 1 || started != 1 || assistants != 1 {
		t.Fatalf("official retry chain changed: attempts=%d scheduled=%d started=%d assistants=%d",
			attempts, scheduled, started, assistants)
	}
	if !strings.Contains(string(inspection.Surface.Messages[len(inspection.Surface.Messages)-1]), "Recovered.") {
		t.Fatalf("retried answer absent from surface: %s", inspection.Surface.Messages[len(inspection.Surface.Messages)-1])
	}
}
