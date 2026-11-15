package sessionformat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The upstream snapshot corpus is optional in normal CI. When present, this
// exercises our current reader against all recorded v4 sessions, not only the
// small set copied into testdata. Upstream snapshots intentionally omit the
// physical seq/time fields and redact cwd/tool schemas; its own corpus runner
// supplies those fields before restoring a Session, so we do the same.
func TestInspectCurrentUpstreamV4SnapshotCorpus(t *testing.T) {
	checkout := os.Getenv("SERVERAGENTS_DEEPSEEK_CHECKOUT")
	if checkout == "" {
		t.Skip("set SERVERAGENTS_DEEPSEEK_CHECKOUT to audit the upstream v4 snapshot corpus")
	}
	root := filepath.Join(checkout, "snapshots")
	var passed int
	var failed []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".v4.jsonl") {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		var normalized bytes.Buffer
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		row := 0
		for scanner.Scan() {
			var record map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				_ = file.Close()
				return fmt.Errorf("%s row %d: %w", path, row, err)
			}
			if row == 0 {
				record["cwd"] = json.RawMessage(`"/tmp/deepseek-corpus-cwd"`)
			} else {
				if string(record["type"]) == `"request/header"` {
					var data map[string]json.RawMessage
					if json.Unmarshal(record["data"], &data) == nil {
						var header map[string]json.RawMessage
						if json.Unmarshal(data["header"], &header) == nil {
							switch {
							case string(header["tools"]) == `"{{tools}}"`:
								delete(header, "tools")
							default:
								// Recorded fixtures scrub full tool schemas down to names.
								// Restore only the shape needed for relationship checks.
								var names []string
								if json.Unmarshal(header["tools"], &names) == nil && names != nil {
									schemas := make([]map[string]any, 0, len(names))
									for _, name := range names {
										schemas = append(schemas, map[string]any{"name": name, "description": "snapshot tool", "parameters": map[string]any{}})
									}
									header["tools"], _ = json.Marshal(schemas)
								}
							}
							data["header"], _ = json.Marshal(header)
							record["data"], _ = json.Marshal(data)
						}
					}
				}
				record["seq"], _ = json.Marshal(row - 1)
				record["time"], _ = json.Marshal(row)
			}
			line, _ := json.Marshal(record)
			normalized.Write(line)
			normalized.WriteByte('\n')
			row++
		}
		closeErr := file.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		inspection, err := InspectV4JSONL(&normalized)
		if err == nil {
			_, err = RestoreLiveSession(inspection.PhysicalArtifact, &memoryV4Journal{})
		}
		if err != nil {
			relative, _ := filepath.Rel(root, path)
			failed = append(failed, fmt.Sprintf("%s: %v", relative, err))
			return nil
		}
		passed++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if passed == 0 && len(failed) == 0 {
		t.Fatal("upstream checkout contains no v4 snapshot sessions")
	}
	for _, failure := range failed {
		t.Log(failure)
	}
	if len(failed) > 0 {
		t.Fatalf("Go reader accepted %d upstream v4 snapshots and rejected %d", passed, len(failed))
	}
	t.Logf("Go reader accepted %d upstream v4 snapshots", passed)
}
