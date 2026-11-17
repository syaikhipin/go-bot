package schedule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"go-bot/internal/durablefs"
)

const maxScheduleDocumentBytes = 64 << 20

// FileStore keeps the Host's Schedule state in its user data tree. It uses
// ordinary file operations in both standalone and NAS-mapped mode.
type FileStore struct{ Path string }

func (s FileStore) path() (string, error) {
	if !filepath.IsAbs(s.Path) || filepath.Base(s.Path) != "schedule.json" {
		return "", errors.New("invalid Host Schedule file path")
	}
	return s.Path, nil
}

func (s FileStore) Load(ctx context.Context) (Document, error) {
	filename, err := s.path()
	if err != nil {
		return Document{}, err
	}
	data, err := readScheduleFile(ctx, filename)
	if errors.Is(err, os.ErrNotExist) {
		return Document{Version: 1, Tasks: []Task{}}, nil
	}
	if err != nil {
		return Document{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Document{}, err
	}
	if _, current := fields["unit"]; current {
		return loadScheduleDomain(data)
	}
	var document Document
	if err := decodeScheduleJSON(data, &document); err != nil {
		return Document{}, err
	}
	if document.Version != 1 {
		return Document{}, fmt.Errorf("unsupported Schedule document version %d", document.Version)
	}
	var tasks []json.RawMessage
	if err := json.Unmarshal(fields["tasks"], &tasks); err != nil || tasks == nil {
		return Document{}, errors.New("Schedule document has no task array")
	}
	if err := validateScheduleDiskShape(tasks, document.Tasks, true); err != nil {
		return Document{}, err
	}
	return document, nil
}

const scheduleDomainVersion = 2

type scheduleUnit struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type scheduleDomain struct {
	Unit   scheduleUnit    `json:"unit"`
	Global json.RawMessage `json:"global"`
	Tables struct {
		Tasks json.RawMessage `json:"tasks"`
	} `json:"tables"`
}

func decodeScheduleJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("Schedule document contains trailing data")
	}
	return nil
}

func loadScheduleDomain(data []byte) (Document, error) {
	var saved scheduleDomain
	if err := decodeScheduleJSON(data, &saved); err != nil {
		return Document{}, err
	}
	if saved.Unit.Name != "schedule" || (saved.Unit.Version != 1 && saved.Unit.Version != scheduleDomainVersion) ||
		string(saved.Global) != "null" || saved.Tables.Tasks == nil {
		return Document{}, errors.New("invalid Schedule domain envelope")
	}
	document := Document{Version: 1, Tasks: []Task{}}
	decoder := json.NewDecoder(bytes.NewReader(saved.Tables.Tasks))
	if start, err := decoder.Token(); err != nil || start != json.Delim('{') {
		return Document{}, errors.New("Schedule domain tasks must be an object")
	}
	var rows []json.RawMessage
	seen := make(map[string]bool)
	for decoder.More() {
		key, err := decoder.Token()
		id, valid := key.(string)
		if err != nil || !valid || id == "" || seen[id] {
			return Document{}, errors.New("invalid or duplicate Schedule task key")
		}
		seen[id] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return Document{}, err
		}
		var task Task
		if err := decodeScheduleJSON(raw, &task); err != nil {
			return Document{}, fmt.Errorf("Schedule task %q: %w", id, err)
		}
		if task.Record.ID != id {
			return Document{}, fmt.Errorf("Schedule task key %q differs from record ID", id)
		}
		rows = append(rows, raw)
		document.Tasks = append(document.Tasks, task)
	}
	if err := validateScheduleDiskShape(rows, document.Tasks, false); err != nil {
		return Document{}, err
	}
	for index := range document.Tasks {
		task := &document.Tasks[index]
		knownDestination := task.SessionID
		if task.Target != nil {
			knownDestination = task.Target.SessionID
		} else if knownDestination != "" {
			task.Target = &Target{Kind: "session", SessionID: knownDestination}
		} else {
			task.Target = &Target{Kind: "new_once"}
		}
		if knownDestination != "" {
			fillReceiptTarget(task, knownDestination)
		}
		if task.Target.Kind == "new_once" && task.Target.SessionID == "" {
			reservation := uuid.NewSHA1(uuid.NameSpaceOID, []byte("go-harness/schedule/"+task.Record.ID))
			task.Target.SessionID = reservation.String()
		}
		task.SessionID = knownDestination
		if task.Status == "inactive" {
			task.Status = "ended"
		}
	}
	return document, nil
}

// The in-memory History zero value also represents a legacy row without a
// history field. On disk, a present but incomplete history is not legacy: the
// upstream domain rejects the whole task table rather than inventing records.
func validateScheduleDiskShape(tasks []json.RawMessage, decoded []Task, legacy bool) error {
	for index, raw := range tasks {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return fmt.Errorf("Schedule task %d is not an object", index)
		}
		if decoded[index].SessionID == "" && legacy {
			return fmt.Errorf("Schedule task %d has no Session ID", index)
		}
		if status, exists := fields["status"]; !exists {
			decoded[index].Status = "active"
		} else if bytes.Equal(status, []byte("null")) {
			return fmt.Errorf("Schedule task %d has null status", index)
		}
		if rawReceipt, exists := fields["lastDelivery"]; exists {
			var receipt map[string]json.RawMessage
			if err := json.Unmarshal(rawReceipt, &receipt); err != nil || receipt == nil {
				return fmt.Errorf("Schedule task %d has invalid last delivery", index)
			}
			if _, exists := receipt["prompt"]; exists {
				return fmt.Errorf("Schedule task %d has a history-only prompt in last delivery", index)
			}
		}
		rawHistory, exists := fields["deliveryHistory"]
		if !exists {
			continue
		}
		var history map[string]json.RawMessage
		if err := json.Unmarshal(rawHistory, &history); err != nil || history == nil {
			return fmt.Errorf("Schedule task %d has invalid delivery history", index)
		}
		records, hasRecords := history["records"]
		unavailable, hasUnavailable := history["earlierRecordsUnavailable"]
		if !hasRecords || !bytes.HasPrefix(bytes.TrimSpace(records), []byte("[")) ||
			!hasUnavailable || (string(unavailable) != "true" && string(unavailable) != "false") {
			return fmt.Errorf("Schedule task %d has incomplete delivery history", index)
		}
		if pruned, exists := history["earlierRecordsPruned"]; exists && string(pruned) != "true" && string(pruned) != "false" {
			return fmt.Errorf("Schedule task %d has invalid history pruning flag", index)
		}
	}
	return nil
}

func readScheduleFile(ctx context.Context, filename string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxScheduleDocumentBytes+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, err
	}
	if len(data) > maxScheduleDocumentBytes {
		return nil, errors.New("Schedule document exceeds 64 MiB")
	}
	return data, nil
}

func (s FileStore) Save(ctx context.Context, document Document) error {
	filename, err := s.path()
	if err != nil {
		return err
	}
	if document.Version != 1 {
		return errors.New("invalid Schedule file write")
	}
	type diskTask struct {
		Task
		DeliveryHistory *History `json:"deliveryHistory,omitempty"`
	}
	domain := scheduleDomain{Unit: scheduleUnit{Name: "schedule", Version: scheduleDomainVersion}, Global: json.RawMessage("null")}
	var table bytes.Buffer
	table.WriteByte('{')
	seen := make(map[string]bool, len(document.Tasks))
	for _, task := range document.Tasks {
		task = cloneTask(task)
		if seen[task.Record.ID] || task.Record.ID == "" {
			return errors.New("invalid or duplicate Schedule ID")
		}
		seen[task.Record.ID] = true
		if task.Target == nil && task.SessionID != "" {
			task.Target = &Target{Kind: "session", SessionID: task.SessionID}
		}
		if task.Target != nil && task.Target.Kind != "new" {
			fillReceiptTarget(&task, task.Target.SessionID)
		}
		if task.Status == "inactive" {
			task.Status = "ended"
		}
		task.SessionID = ""
		row := diskTask{Task: task}
		if task.DeliveryHistory.Records != nil || task.DeliveryHistory.EarlierRecordsUnavailable || task.DeliveryHistory.EarlierRecordsPruned {
			history := task.DeliveryHistory
			row.DeliveryHistory = &history
		}
		value, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if table.Len() > 1 {
			table.WriteByte(',')
		}
		key, _ := json.Marshal(task.Record.ID)
		table.Write(key)
		table.WriteByte(':')
		table.Write(value)
	}
	table.WriteByte('}')
	domain.Tables.Tasks = table.Bytes()
	data, err := json.MarshalIndent(domain, "", "  ")
	data = append(data, '\n')
	if err != nil {
		return err
	}
	if len(data) > maxScheduleDocumentBytes {
		return errors.New("Schedule document exceeds 64 MiB")
	}
	if err := putScheduleFile(ctx, filename, data); err != nil {
		// A directory fsync error is not made successful by a read-back that
		// may only reflect the local NFS client cache.
		return err
	}
	readback, err := readScheduleFile(ctx, filename)
	if err != nil {
		return fmt.Errorf("verify Schedule file: %w", err)
	}
	if !bytes.Equal(readback, data) {
		return errors.New("Schedule file did not match the committed task state")
	}
	return nil
}

func putScheduleFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := durablefs.EnsureDirectory(directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".schedule-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	return durablefs.SyncDirectory(directory)
}
