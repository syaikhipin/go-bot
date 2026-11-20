// Package credentials implements the Host's built-in credential provider.
// Plugins use this service through an adapter; neither Cordis nor the MCP
// client owns a second credential database.
package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go-bot/internal/durablefs"
	"gopkg.in/yaml.v3"
)

const Filename = ".credentials.yaml"
const documentLimit = 4 << 20
const recordLimit = 512 << 10

var refPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*/[a-z][a-z0-9-]*$`)

func ValidRef(ref string) bool { return refPattern.MatchString(ref) }
func ValidKey(key string) bool { return keyPattern.MatchString(key) }

type Record struct {
	Kind    string            `json:"kind" yaml:"kind"`
	Key     *string           `json:"key,omitempty" yaml:"key,omitempty"`
	Env     map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Payload json.RawMessage   `json:"payload,omitempty" yaml:"-"`
}

// Reject unknown fields at every JSON ingress, including Node callback results,
// rather than silently dropping part of a purportedly lossless record.
func (r *Record) UnmarshalJSON(data []byte) error {
	type wire Record
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return errors.New("invalid credential record")
	}
	if !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return errors.New("invalid credential record")
	}
	next := Record(value)
	if err := next.Validate(); err != nil {
		return err
	}
	*r = next
	return nil
}

func (r Record) Validate() error {
	switch r.Kind {
	case "api-key":
		if r.Payload != nil || (r.Key != nil && *r.Key == "") {
			return errors.New("invalid api-key credential record")
		}
		for name := range r.Env {
			if !ValidRef(name) {
				return errors.New("invalid credential environment name")
			}
		}
	case "grant":
		if r.Key != nil || r.Env != nil || !json.Valid(r.Payload) {
			return errors.New("invalid grant credential record")
		}
	default:
		return errors.New("unknown credential record kind")
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > recordLimit {
		return errors.New("credential record exceeds supported limits")
	}
	return nil
}

type Resolved struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}
type Info struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Writable   bool   `json:"writable"`
	Masked     string `json:"masked,omitempty"`
}
type RecordInfo struct {
	Configured bool   `json:"configured"`
	Kind       string `json:"kind,omitempty"`
	Writable   bool   `json:"writable"`
}
type RecordEntry struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
}
type Update struct {
	References []string `json:"references"`
	Records    []string `json:"records"`
}
type document struct {
	refs    map[string]string
	records map[string]Record
	node    *yaml.Node
}
type Options struct {
	Watch bool
	Warn  func(error)
}

// Store serializes all writes and read-modify-write callbacks in the one Go
// Host. Reads expose committed snapshots, so a mutation callback can inspect
// other committed records without re-entering its own mutation queue.
type Store struct {
	path      string
	gate      chan struct{}
	mu        sync.RWMutex
	doc       document
	provided  map[string]string
	observers map[uint64]func(Update)
	secrets   map[string]bool
	next      uint64
	closed    bool
	stop      chan struct{}
	watchDone chan struct{}
	closeOnce sync.Once
	warn      func(error)
}

func Open(path string, options Options) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("credential path must be absolute")
	}
	s := &Store{path: filepath.Clean(path), gate: make(chan struct{}, 1), provided: map[string]string{}, observers: map[uint64]func(Update){}, stop: make(chan struct{}), watchDone: make(chan struct{}), warn: options.Warn}
	doc, err := readDocument(s.path)
	if err != nil {
		return nil, err
	}
	s.doc = doc
	s.rememberLocked(doc)
	if options.Watch {
		go s.watch()
	} else {
		close(s.watchDone)
	}
	return s, nil
}

func emptyDocument() document {
	return document{refs: map[string]string{}, records: map[string]Record{}, node: &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}}
}

func readDocument(path string) (document, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyDocument(), nil
	}
	if err != nil {
		return document{}, errors.New("cannot read Host credential document")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > documentLimit {
		return document{}, errors.New("Host credential document must be a private regular file up to 4 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, documentLimit+1))
	if err != nil || len(data) > documentLimit {
		return document{}, errors.New("cannot read Host credential document")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return emptyDocument(), nil
	}
	doc := emptyDocument()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(doc.node) != nil || len(doc.node.Content) != 1 || doc.node.Content[0].Kind != yaml.MappingNode || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		return document{}, errors.New("invalid Host credential document")
	}
	var raw struct {
		Refs    map[string]string `yaml:"refs"`
		Records map[string]any    `yaml:"records"`
	}
	// Node.Decode also rejects duplicate mapping keys. Inspect node tags below
	// so a number/bool is never silently coerced into a secret string.
	if doc.node.Decode(&raw) != nil {
		return document{}, errors.New("invalid Host credential document")
	}
	root := doc.node.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		name, value := root.Content[i].Value, root.Content[i+1]
		if name != "refs" && name != "records" {
			return document{}, errors.New("unknown Host credential document field")
		}
		if value.Kind != yaml.MappingNode {
			return document{}, errors.New("credential sections must be mappings")
		}
		if name == "refs" {
			for j := 0; j < len(value.Content); j += 2 {
				if !ValidRef(value.Content[j].Value) || value.Content[j+1].Kind != yaml.ScalarNode || value.Content[j+1].Tag != "!!str" {
					return document{}, errors.New("invalid stored credential reference")
				}
			}
		}
	}
	for ref, value := range raw.Refs {
		if value != "" {
			doc.refs[ref] = value
		}
	}
	for key, value := range raw.Records {
		if !ValidKey(key) {
			return document{}, errors.New("invalid stored credential key")
		}
		data, err := json.Marshal(value)
		if err != nil {
			return document{}, errors.New("credential record must be JSON")
		}
		var record Record
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if dec.Decode(&record) != nil || record.Validate() != nil {
			return document{}, errors.New("invalid stored credential record")
		}
		doc.records[key] = cloneRecord(record)
	}
	return doc, nil
}

func cloneRecord(record Record) Record {
	data, _ := json.Marshal(record)
	var copy Record
	_ = json.Unmarshal(data, &copy)
	if copy.Kind == "grant" {
		decoder := json.NewDecoder(bytes.NewReader(copy.Payload))
		decoder.UseNumber()
		var payload any
		if decoder.Decode(&payload) == nil {
			copy.Payload, _ = json.Marshal(payload)
		}
	}
	return copy
}
func cloneRecords(records map[string]Record) map[string]Record {
	out := make(map[string]Record, len(records))
	for k, v := range records {
		out[k] = cloneRecord(v)
	}
	return out
}
func cloneStrings(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

// SetProvided installs the Controller-supplied fallback layer in Go memory.
// It is not written into the managed document. SDK set/unset owns that file,
// which, as upstream, outranks supplied fallback values.
func (s *Store) SetProvided(values map[string]string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	before := s.provided
	s.provided = cloneStrings(values)
	for _, value := range values {
		if value != "" {
			s.secrets[value] = true
		}
	}
	update := Update{}
	for key, value := range before {
		if values[key] != value {
			update.References = append(update.References, key)
		}
	}
	for key, value := range values {
		if before[key] != value {
			update.References = append(update.References, key)
		}
	}
	s.mu.Unlock()
	s.publish(update)
}

// Lookup also supports existing Go Bot credential-reference aliases. Standard
// resolve/describe validates the upstream POSIX reference grammar at the RPC
// adapter; aliases never become entries in the upstream-format document.
func (s *Store) Lookup(ref string) (*Resolved, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("credential service is closed")
	}
	if value := s.doc.refs[ref]; value != "" {
		return &Resolved{value, "file"}, nil
	}
	if value := s.provided[ref]; value != "" {
		return &Resolved{value, "controller"}, nil
	}
	return nil, nil
}
func (s *Store) Resolve(ref string) (*Resolved, error) {
	if !ValidRef(ref) {
		return nil, errors.New("invalid credential reference")
	}
	return s.Lookup(ref)
}
func (s *Store) Describe(ref string) (Info, error) {
	value, err := s.Resolve(ref)
	if err != nil {
		return Info{}, err
	}
	info := Info{Writable: true}
	if value != nil {
		info.Configured = true
		info.Source = value.Source
		if value.Source == "file" {
			info.Masked = "********"
		}
	}
	return info, nil
}
func (s *Store) Set(ctx context.Context, ref, value string) error {
	if !ValidRef(ref) || value == "" || len(value) > 16384 || strings.ContainsRune(value, 0) {
		return errors.New("invalid nonempty credential value; use unset to remove it")
	}
	return s.mutate(ctx, func(doc *document) error { doc.refs[ref] = value; return nil })
}
func (s *Store) Unset(ctx context.Context, ref string) error {
	if !ValidRef(ref) {
		return errors.New("invalid credential reference")
	}
	return s.mutate(ctx, func(doc *document) error { delete(doc.refs, ref); return nil })
}
func (s *Store) ReadRecord(key string) (*Record, error) {
	if !ValidKey(key) {
		return nil, errors.New("invalid credential key")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("credential service is closed")
	}
	value, ok := s.doc.records[key]
	if !ok {
		return nil, nil
	}
	copy := cloneRecord(value)
	return &copy, nil
}
func (s *Store) DescribeRecord(key string) (RecordInfo, error) {
	value, err := s.ReadRecord(key)
	info := RecordInfo{Writable: true}
	if value != nil {
		info.Configured = true
		info.Kind = value.Kind
	}
	return info, err
}
func (s *Store) ListRecords() ([]RecordEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("credential service is closed")
	}
	rows := make([]RecordEntry, 0, len(s.doc.records))
	for key, value := range s.doc.records {
		rows = append(rows, RecordEntry{key, value.Kind})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows, nil
}
func (s *Store) ModifyRecord(ctx context.Context, key string, mutate func(*Record) (*Record, error)) (*Record, error) {
	if !ValidKey(key) || mutate == nil {
		return nil, errors.New("invalid credential mutation")
	}
	var result *Record
	update := Update{}
	err := s.mutateWithUpdates(ctx, func(doc *document) error {
		var current *Record
		if value, ok := doc.records[key]; ok {
			copy := cloneRecord(value)
			current = &copy
		}
		next, err := mutate(current)
		if err != nil {
			return err
		}
		if next == nil {
			result = current
			return nil
		}
		if err := next.Validate(); err != nil {
			return err
		}
		copy := cloneRecord(*next)
		doc.records[key] = copy
		update.Records = []string{key}
		detached := cloneRecord(copy)
		result = &detached
		return nil
	}, &update)
	return result, err
}
func (s *Store) DeleteRecord(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("invalid credential key")
	}
	return s.mutate(ctx, func(doc *document) error { delete(doc.records, key); return nil })
}

// UpdateRecords is the internal Go counterpart of the serialized callback.
// Returning an error aborts every change; unrelated refs/records are retained.
func (s *Store) UpdateRecords(ctx context.Context, fn func(map[string]Record) error) error {
	return s.mutate(ctx, func(doc *document) error {
		if err := fn(doc.records); err != nil {
			return err
		}
		for key, record := range doc.records {
			if !ValidKey(key) {
				return errors.New("invalid credential key")
			}
			if err := record.Validate(); err != nil {
				return err
			}
		}
		return nil
	})
}

func changes(before, after document) Update {
	u := Update{}
	for key, value := range before.refs {
		if after.refs[key] != value {
			u.References = append(u.References, key)
		}
	}
	for key, value := range after.refs {
		if before.refs[key] != value {
			u.References = append(u.References, key)
		}
	}
	for key, value := range before.records {
		if !reflect.DeepEqual(after.records[key], value) {
			u.Records = append(u.Records, key)
		}
	}
	for key, value := range after.records {
		if !reflect.DeepEqual(before.records[key], value) {
			u.Records = append(u.Records, key)
		}
	}
	return u
}
func (s *Store) mutate(ctx context.Context, fn func(*document) error) error {
	return s.mutateWithUpdates(ctx, fn, nil)
}

// Explicit record writes notify after commit even when the value is unchanged,
// matching the upstream provider's authorization commit-observation contract.
func (s *Store) mutateWithUpdates(ctx context.Context, fn func(*document) error, forced *Update) error {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("credential service is closed")
	}
	locked := true
	defer func() {
		if locked {
			<-s.gate
		}
	}()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return errors.New("credential service is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	doc, err := readDocument(s.path)
	if err != nil {
		return err
	}
	before := document{refs: cloneStrings(doc.refs), records: cloneRecords(doc.records)}
	if err = fn(&doc); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	u := changes(before, doc)
	if forced != nil {
		u.References = append(u.References, forced.References...)
		u.Records = append(u.Records, forced.Records...)
	}
	if len(u.References)+len(u.Records) > 0 {
		if err = s.write(ctx, &doc); err != nil {
			return err
		}
	}
	s.mu.Lock()
	prior := s.doc
	s.doc = doc
	s.rememberLocked(doc)
	s.mu.Unlock()
	u = changes(prior, doc)
	if forced != nil {
		u.References = append(u.References, forced.References...)
		u.Records = append(u.Records, forced.Records...)
	}
	<-s.gate
	locked = false
	s.publish(u)
	return nil
}

// Update the YAML nodes for changed values rather than re-serializing the whole
// document; unrelated comments, entry order and opaque grant fields survive.
func section(root *yaml.Node, name string) *yaml.Node {
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == name {
			return root.Content[i+1]
		}
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, node)
	return node
}
func syncMapping(node *yaml.Node, values map[string]any) error {
	seen := map[string]bool{}
	for i := 0; i < len(node.Content); {
		key := node.Content[i].Value
		value, ok := values[key]
		if !ok {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			continue
		}
		seen[key] = true
		var next yaml.Node
		if err := next.Encode(value); err != nil {
			return err
		}
		var old any
		if err := node.Content[i+1].Decode(&old); err != nil {
			return err
		}
		oldJSON, oldErr := json.Marshal(old)
		nextJSON, nextErr := json.Marshal(value)
		if oldErr != nil || nextErr != nil || !bytes.Equal(oldJSON, nextJSON) {
			next.HeadComment = node.Content[i+1].HeadComment
			next.LineComment = node.Content[i+1].LineComment
			next.FootComment = node.Content[i+1].FootComment
			node.Content[i+1] = &next
		}
		i += 2
	}
	keys := make([]string, 0)
	for key := range values {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		var next yaml.Node
		if err := next.Encode(values[key]); err != nil {
			return err
		}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &next)
	}
	return nil
}
func (s *Store) write(ctx context.Context, doc *document) error {
	refs := map[string]any{}
	for key, value := range doc.refs {
		refs[key] = value
	}
	records := map[string]any{}
	for key, value := range doc.records {
		data, _ := json.Marshal(value)
		var raw any
		if json.Unmarshal(data, &raw) != nil {
			return errors.New("credential record must be JSON")
		}
		records[key] = raw
	}
	if err := syncMapping(section(doc.node.Content[0], "refs"), refs); err != nil {
		return err
	}
	if err := syncMapping(section(doc.node.Content[0], "records"), records); err != nil {
		return err
	}
	data, err := yaml.Marshal(doc.node)
	if err != nil || len(data) > documentLimit {
		return errors.New("Host credential document exceeds supported limits")
	}
	directory := filepath.Dir(s.path)
	if err = durablefs.EnsureDirectory(directory); err != nil {
		return errors.New("cannot create private credential directory")
	}
	file, err := os.CreateTemp(directory, ".credential-write-")
	if err != nil {
		return errors.New("cannot prepare credential write")
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err = errors.Join(writeErr, file.Close(), ctx.Err()); err != nil {
		return errors.New("cannot persist Host credentials")
	}
	if err = os.Rename(file.Name(), s.path); err != nil {
		return errors.New("cannot publish Host credentials")
	}
	return durablefs.SyncDirectory(directory)
}

func (s *Store) Subscribe(observer func(Update)) func() {
	s.mu.Lock()
	s.next++
	id := s.next
	if !s.closed {
		s.observers[id] = observer
	}
	s.mu.Unlock()
	return func() { s.mu.Lock(); delete(s.observers, id); s.mu.Unlock() }
}
func (s *Store) publish(update Update) {
	unique := func(rows []string) []string {
		sort.Strings(rows)
		out := rows[:0]
		for _, key := range rows {
			if len(out) == 0 || out[len(out)-1] != key {
				out = append(out, key)
			}
		}
		return out
	}
	update.References = unique(update.References)
	update.Records = unique(update.Records)
	if len(update.References)+len(update.Records) == 0 {
		return
	}
	s.mu.RLock()
	observers := make([]func(Update), 0, len(s.observers))
	for _, observer := range s.observers {
		observers = append(observers, observer)
	}
	s.mu.RUnlock()
	for _, observer := range observers {
		func() {
			defer func() {
				if recover() != nil && s.warn != nil {
					s.warn(errors.New("credential observer failed"))
				}
			}()
			observer(Update{append([]string(nil), update.References...), append([]string(nil), update.Records...)})
		}()
	}
}
func (s *Store) watch() {
	defer close(s.watchDone)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var previous os.FileInfo
	initialized := false
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			select {
			case s.gate <- struct{}{}:
			default:
				continue
			}
			info, statErr := os.Stat(s.path)
			same := initialized && ((errors.Is(statErr, os.ErrNotExist) && previous == nil) || (statErr == nil && previous != nil && os.SameFile(info, previous) && info.Size() == previous.Size() && info.ModTime().Equal(previous.ModTime()) && info.Mode() == previous.Mode()))
			if same {
				<-s.gate
				continue
			}
			// Save the signature from before the read. A concurrent atomic replacement
			// during loading is then observed on the next tick, closing the startup gap.
			previous = info
			initialized = true
			doc, err := readDocument(s.path)
			if err == nil {
				s.mu.Lock()
				prior := s.doc
				s.doc = doc
				s.rememberLocked(doc)
				s.mu.Unlock()
				<-s.gate
				s.publish(changes(prior, doc))
			} else {
				<-s.gate
				if s.warn != nil {
					s.warn(fmt.Errorf("credential reload failed: %w", err))
				}
			}
		}
	}
}
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.stop)
		<-s.watchDone
		s.gate <- struct{}{}
		<-s.gate
	})
	return nil
}

func (s *Store) rememberLocked(doc document) {
	if s.secrets == nil {
		s.secrets = map[string]bool{}
	}
	for _, value := range doc.refs {
		if value != "" {
			s.secrets[value] = true
		}
	}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case string:
			if value != "" {
				s.secrets[value] = true
			}
		case []any:
			for _, v := range value {
				visit(v)
			}
		case map[string]any:
			for _, v := range value {
				visit(v)
			}
		}
	}
	for _, record := range doc.records {
		if record.Key != nil && *record.Key != "" {
			s.secrets[*record.Key] = true
		}
		for _, value := range record.Env {
			if value != "" {
				s.secrets[value] = true
			}
		}
		if record.Kind == "grant" {
			var payload any
			if json.Unmarshal(record.Payload, &payload) == nil {
				visit(payload)
			}
		}
	}
}
func (s *Store) Redact(text string) string {
	s.mu.RLock()
	values := make([]string, 0, len(s.secrets))
	for value := range s.secrets {
		values = append(values, value)
	}
	s.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return text
}
