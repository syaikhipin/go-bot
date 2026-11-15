package sessionformat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A crash can leave any byte prefix of the next JSONL batch. Every complete
// row remains part of the Session; only the final unterminated row is removed
// on the next nonempty append. Sweep the full tool lifecycle rather than a
// single hand-picked tear position.
func TestFileStoreTornToolExchangeAtEveryByteOffset(t *testing.T) {
	header := v4TestHeader("torn-matrix")
	header.CWD = "/root/harness/workspaces/torn-matrix"
	headerLine, err := EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	base := append(headerLine, '\n')
	rows := coreToolExchange()
	rows[4].Data = bytes.Replace(rows[4].Data, []byte("done"), []byte("完成✅"), 1)
	var payload []byte
	ends := []int{0}
	for _, event := range rows {
		line, err := EncodeEvent(event, event.Seq)
		if err != nil {
			t.Fatal(err)
		}
		payload = append(payload, line...)
		payload = append(payload, '\n')
		ends = append(ends, len(payload))
	}
	if _, err := InspectV4JSONL(bytes.NewReader(append(bytes.Clone(base), payload...))); err != nil {
		t.Fatalf("tool exchange fixture is not a valid v4 log: %v", err)
	}

	physicalCuts := map[int]bool{0: true, len(payload): true}
	for index := 1; index < len(ends); index++ {
		start, end := ends[index-1], ends[index]
		physicalCuts[start] = true
		physicalCuts[start+(end-start)/2] = true
		physicalCuts[end-1] = true
		physicalCuts[end] = true
	}
	for cut := 0; cut <= len(payload); cut++ {
		committedRows, committedBytes := 0, 0
		for index := 1; index < len(ends) && ends[index] <= cut; index++ {
			committedRows, committedBytes = index, ends[index]
		}
		raw := append(bytes.Clone(base), payload[:cut]...)
		inspection, repairAt, err := inspectRecoverableFileLog(raw)
		if err != nil {
			t.Fatalf("cut %d rejected: %v", cut, err)
		}
		if len(inspection.Events) != committedRows {
			t.Fatalf("cut %d retained %d rows, want %d", cut, len(inspection.Events), committedRows)
		}
		wantRepair := -1
		if committedBytes != cut {
			wantRepair = len(base) + committedBytes
		}
		if repairAt != wantRepair {
			t.Fatalf("cut %d repair offset = %d, want %d", cut, repairAt, wantRepair)
		}
		if !physicalCuts[cut] {
			continue
		}
		t.Run(fmt.Sprintf("cut-%d", cut), func(t *testing.T) {
			ctx := context.Background()
			store := FileStore{Root: filepath.Join(t.TempDir(), "sessions"), ProjectCWD: header.CWD, Claim: v4Claim}
			journal, err := store.Create(ctx, header)
			if err != nil {
				t.Fatal(err)
			}
			path := journal.Path()
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(payload[:cut]); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, artifact, err := store.Open(ctx, header.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifact.Events) != committedRows {
				_ = reopened.Close()
				t.Fatalf("physical cut %d retained %d rows, want %d", cut, len(artifact.Events), committedRows)
			}
			live, err := RestoreLiveSession(artifact, reopened)
			if err != nil {
				_ = reopened.Close()
				t.Fatal(err)
			}
			if err := live.Close(); err != nil {
				t.Fatal(err)
			}
			final, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(final, []byte{'\n'}) {
				t.Fatalf("repaired log at cut %d still has an unterminated tail", cut)
			}
			finalInspection, err := InspectV4JSONL(bytes.NewReader(final))
			if err != nil {
				t.Fatalf("repaired log at cut %d is not v4: %v", cut, err)
			}
			if len(finalInspection.Events) <= committedRows {
				t.Fatalf("cut %d did not persist its recovery suffix", cut)
			}
			for index := 0; index < committedRows; index++ {
				if got, want := finalInspection.Events[index], rows[index]; got.Type != want.Type || !bytes.Equal(got.Data, want.Data) {
					t.Fatalf("cut %d changed committed row %d: %+v", cut, index, got)
				}
			}
			if err := ValidateV4Relationships(finalInspection.Events); err != nil {
				t.Fatalf("cut %d produced invalid v4 relationships: %v", cut, err)
			}
		})
	}
}
