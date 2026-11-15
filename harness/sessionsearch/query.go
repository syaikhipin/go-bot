package sessionsearch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Query struct {
	Text      string  `json:"query"`
	SessionID string  `json:"session_id"`
	Anchor    *uint64 `json:"around_message_id"`
	Limit     int     `json:"limit"`
	Window    int     `json:"window"`
	Roles     string  `json:"role_filter"`
	After     string  `json:"after"`
	Before    string  `json:"before"`
}

type Result struct {
	Mode      string    `json:"mode"`
	Sessions  []Session `json:"sessions,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Messages  []Message `json:"messages,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

type Session struct {
	ID        string   `json:"session_id"`
	Title     string   `json:"title,omitempty"`
	CreatedAt int64    `json:"created_at"`
	Match     *Message `json:"match,omitempty"`
}

type Message struct {
	ID        uint64 `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (q Query) Validate() error {
	if len(q.Text) > 2048 || q.Limit < 0 || q.Limit > 10 || q.Window < 0 || q.Window > 20 {
		return errors.New("query must be at most 2048 bytes, limit at most 10, and window at most 20")
	}
	if q.SessionID != "" && uuid.Validate(q.SessionID) != nil {
		return errors.New("session_id must be a Session UUID")
	}
	if q.Anchor != nil && (q.SessionID == "" || *q.Anchor > (1<<53)-1) {
		return errors.New("around_message_id requires a session_id and a valid v4 sequence")
	}
	for _, role := range strings.Split(q.Roles, ",") {
		switch strings.TrimSpace(role) {
		case "", "user", "assistant":
		default:
			return errors.New("role_filter supports user and assistant")
		}
	}
	for _, value := range []string{q.After, q.Before} {
		if value != "" {
			if _, err := parseDate(value); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseDate(value string) (int64, error) {
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UnixMilli(), nil
	}
	if parsed, err := time.Parse(time.DateOnly, value); err == nil {
		return parsed.UnixMilli(), nil
	}
	return 0, errors.New("after and before must be ISO dates or RFC3339 timestamps")
}

func (i *Index) Query(ctx context.Context, current string, q Query) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.sync(ctx); err != nil {
		return Result{}, err
	}
	switch {
	case q.SessionID != "":
		return i.read(ctx, current, q)
	case strings.TrimSpace(q.Text) != "":
		return i.discover(ctx, current, q)
	default:
		return i.browse(ctx, current, q.Limit)
	}
}

func (i *Index) discover(ctx context.Context, current string, q Query) (Result, error) {
	limit := q.Limit
	if limit == 0 {
		limit = 3
	}
	conditions := []string{"message_fts MATCH ?", "s.id <> ?"}
	args := []any{ftsQuery(q.Text), current}
	if strings.TrimSpace(q.Roles) != "" {
		roles := "," + strings.Join(strings.FieldsFunc(q.Roles, unicode.IsSpace), "") + ","
		conditions = append(conditions, "instr(?, ',' || m.role || ',') > 0")
		args = append(args, roles)
	}
	if q.After != "" {
		after, _ := parseDate(q.After)
		conditions = append(conditions, "s.created >= ?")
		args = append(args, after)
	}
	if q.Before != "" {
		before, _ := parseDate(q.Before)
		conditions = append(conditions, "s.created < ?")
		args = append(args, before)
	}
	args = append(args, limit)
	rows, err := i.db.QueryContext(ctx, `
WITH hits AS MATERIALIZED (
	SELECT s.id, s.created, m.seq, m.role, m.body, m.time, bm25(message_fts) AS score,
	       snippet(message_fts, 0, char(1), char(2), char(3), 24) AS excerpt
	FROM message_fts JOIN messages m ON m.id = message_fts.rowid
	JOIN sessions s ON s.id = m.session_id
	WHERE `+strings.Join(conditions, " AND ")+`
), ranked AS (
	SELECT *, row_number() OVER (PARTITION BY id ORDER BY score, time DESC, seq DESC) AS rank FROM hits
)
SELECT id, created, seq, role, body, time, excerpt FROM ranked WHERE rank = 1
ORDER BY score, time DESC, id LIMIT ?`, args...)
	if err != nil {
		return Result{}, fmt.Errorf("FTS5 query failed: %w", err)
	}
	defer rows.Close()

	result := Result{Mode: "discover", Sessions: []Session{}}
	for rows.Next() {
		var item Session
		var match Message
		var excerpt string
		if err := rows.Scan(&item.ID, &item.CreatedAt, &match.ID, &match.Role, &match.Content, &match.Timestamp, &excerpt); err != nil {
			return Result{}, err
		}
		match.preview(excerpt)
		item.Match = &match
		i.setTitle(&item)
		result.Sessions = append(result.Sessions, item)
	}
	return result, rows.Err()
}

func (i *Index) browse(ctx context.Context, current string, limit int) (Result, error) {
	if limit == 0 {
		limit = 10
	}
	rows, err := i.db.QueryContext(ctx, `SELECT s.id, s.created FROM sessions s
WHERE s.id <> ? ORDER BY coalesce((SELECT max(m.time) FROM messages m WHERE m.session_id = s.id), s.created) DESC, s.id LIMIT ?`, current, limit)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()
	result := Result{Mode: "browse", Sessions: []Session{}}
	for rows.Next() {
		var item Session
		if err := rows.Scan(&item.ID, &item.CreatedAt); err != nil {
			return Result{}, err
		}
		i.setTitle(&item)
		result.Sessions = append(result.Sessions, item)
	}
	return result, rows.Err()
}

func (i *Index) setTitle(item *Session) {
	if i.source.Title != nil {
		item.Title = i.source.Title(item.ID)
	}
}

func (i *Index) read(ctx context.Context, current string, q Query) (Result, error) {
	var present int
	err := i.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ? AND id <> ?", q.SessionID, current).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, errors.New("historical session is unavailable")
	}
	if err != nil {
		return Result{}, err
	}
	result := Result{Mode: "read", SessionID: q.SessionID, Messages: []Message{}}
	if q.Anchor != nil {
		result.Mode = "scroll"
		window := q.Window
		if window == 0 {
			window = 5
		}
		before, err := i.messages(ctx, q.SessionID, "seq < ?", "DESC", window, *q.Anchor)
		if err != nil {
			return Result{}, err
		}
		for j := len(before) - 1; j >= 0; j-- {
			result.Messages = append(result.Messages, before[j])
		}
		after, err := i.messages(ctx, q.SessionID, "seq >= ?", "ASC", window+1, *q.Anchor)
		if err != nil {
			return Result{}, err
		}
		if len(after) == 0 || after[0].ID != *q.Anchor {
			return Result{}, errors.New("message anchor is unavailable in this session")
		}
		result.Messages = append(result.Messages, after...)
		return result, nil
	}
	first, err := i.messages(ctx, q.SessionID, "1", "ASC", 21)
	if err != nil {
		return Result{}, err
	}
	if len(first) <= 20 {
		result.Messages = append(result.Messages, first...)
		return result, nil
	}
	last, err := i.messages(ctx, q.SessionID, "1", "DESC", 10)
	if err != nil {
		return Result{}, err
	}
	result.Truncated = true
	result.Messages = append(result.Messages, first[:10]...)
	for j := len(last) - 1; j >= 0; j-- {
		result.Messages = append(result.Messages, last[j])
	}
	return result, nil
}

func (i *Index) messages(ctx context.Context, id, where, order string, limit int, values ...any) ([]Message, error) {
	args := append([]any{id}, values...)
	args = append(args, limit)
	rows, err := i.db.QueryContext(ctx, "SELECT seq, role, body, time FROM messages WHERE session_id = ? AND "+where+" ORDER BY seq "+order+" LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.Timestamp); err != nil {
			return nil, err
		}
		m.bound(1600)
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (m *Message) bound(limit int) {
	count := 0
	for index := range m.Content {
		if count == limit {
			m.Content = m.Content[:index]
			m.Truncated = true
			return
		}
		count++
	}
}

func (m *Message) preview(excerpt string) {
	// FTS5 highlights the segmented text. Map its first hit back to an
	// original byte offset so the preview includes the hit without added spaces.
	if utf8.RuneCountInString(m.Content) <= 900 {
		return
	}
	if prefix, _, ok := strings.Cut(excerpt, "\x01"); ok {
		fragment := strings.NewReplacer("\x01", "", "\x02", "", "\x03", "").Replace(excerpt)
		segmented := segmentHan(m.Content)
		if start := strings.Index(segmented, fragment); fragment != "" && start >= 0 {
			offset := start + len(strings.TrimPrefix(prefix, "\x03"))
			var previous rune
			var position int
			for index, r := range m.Content {
				if separatesHan(previous, r) {
					position++
				}
				if position >= offset {
					m.Content = m.Content[index:]
					m.Truncated = index != 0
					break
				}
				position += utf8.RuneLen(r)
				previous = r
			}
		}
	}
	m.bound(900)
}

func separatesHan(previous, next rune) bool {
	return previous != 0 && !unicode.IsSpace(previous) && !unicode.IsSpace(next) &&
		(unicode.Is(unicode.Han, previous) || unicode.Is(unicode.Han, next))
}

func segmentHan(text string) string {
	var result strings.Builder
	var previous rune
	for _, r := range text {
		if separatesHan(previous, r) {
			result.WriteByte(' ')
		}
		result.WriteRune(r)
		previous = r
	}
	return result.String()
}

// Rewrite whole FTS operands only. Preserve boolean operators, parentheses,
// quoted phrases and prefix syntax. A bare Chinese operand becomes a phrase,
// so adjacent characters match in order.
func ftsQuery(text string) string {
	var result strings.Builder
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		switch {
		case r == '"':
			end := size
			for end < len(text) {
				if text[end] == '"' {
					if end+1 < len(text) && text[end+1] == '"' {
						end += 2
						continue
					}
					break
				}
				end++
			}
			result.WriteByte('"')
			result.WriteString(segmentHan(text[size:end]))
			if end < len(text) {
				result.WriteByte('"')
				end++
			}
			text = text[end:]
		case unicode.IsSpace(r) || strings.ContainsRune("()+*:^,{}-", r):
			result.WriteString(text[:size])
			text = text[size:]
		default:
			end := size
			for end < len(text) {
				next, n := utf8.DecodeRuneInString(text[end:])
				if unicode.IsSpace(next) || strings.ContainsRune("\"()+*:^,{}-", next) {
					break
				}
				end += n
			}
			word := text[:end]
			if strings.ContainsFunc(word, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
				result.WriteByte('"')
				result.WriteString(segmentHan(word))
				result.WriteByte('"')
			} else {
				result.WriteString(word)
			}
			text = text[end:]
		}
	}
	return result.String()
}
