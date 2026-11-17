package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"go-bot/harness/session"

	"github.com/google/uuid"
)

func defaultSessionWorkspace(root, id string) string {
	return filepath.Join(root, "workspaces", id)
}

func topLevelSession(header sessionformat.Header) bool {
	return header.ParentSession == "" && header.Origin != "subagent"
}

// findHostSession locates a journal by Session ID independently of its cwd.
// The traditional per-chat project path remains the common fast path.
func findHostSession(ctx context.Context, root, id string) (sessionformat.Header, error) {
	if uuid.Validate(id) != nil {
		return sessionformat.Header{}, os.ErrNotExist
	}
	logs := sessionLogRoot(root)
	fallback := defaultSessionWorkspace(root, id)
	key, err := sessionformat.ProjectKey(fallback)
	if err != nil {
		return sessionformat.Header{}, err
	}
	header, err := sessionformat.ReadProjectHeader(ctx, logs, key, id)
	if err == nil && topLevelSession(header) {
		return header, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return sessionformat.Header{}, err
	}
	projects, err := os.ReadDir(logs)
	if err != nil {
		return sessionformat.Header{}, err
	}
	for _, project := range projects {
		if !project.IsDir() || project.Name() == key {
			continue
		}
		header, err := sessionformat.ReadProjectHeader(ctx, logs, project.Name(), id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return sessionformat.Header{}, err
		}
		if topLevelSession(header) {
			return header, nil
		}
	}
	return sessionformat.Header{}, os.ErrNotExist
}

// listHostSessions discovers conversations from durable journals. Several
// top-level Session IDs can share one project and its files.
func listHostSessions(ctx context.Context, root, cursor string, limit int) (sessionformat.SessionListPage, error) {
	if (cursor != "" && uuid.Validate(cursor) != nil) || limit < 1 || limit > 100 {
		return sessionformat.SessionListPage{}, errors.New("invalid Host Session page request")
	}
	logs := sessionLogRoot(root)
	candidates, err := hostSessionCandidates(ctx, logs, cursor)
	if err != nil {
		return sessionformat.SessionListPage{}, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
	page := sessionformat.SessionListPage{Sessions: make([]sessionformat.Header, 0, limit)}
	for _, entry := range candidates {
		header, ok, err := unarchivedHostSession(ctx, logs, entry)
		if err != nil {
			return sessionformat.SessionListPage{}, err
		}
		if !ok {
			continue
		}
		if len(page.Sessions) == limit {
			page.NextCursor = page.Sessions[len(page.Sessions)-1].ID
			break
		}
		page.Sessions = append(page.Sessions, header)
	}
	return page, nil
}

func latestHostSession(ctx context.Context, root string) (sessionformat.SessionListPage, error) {
	logs := sessionLogRoot(root)
	candidates, err := hostSessionCandidates(ctx, logs, "")
	if err != nil {
		return sessionformat.SessionListPage{}, err
	}
	var latest sessionformat.Header
	for _, entry := range candidates {
		header, ok, err := unarchivedHostSession(ctx, logs, entry)
		if err != nil {
			return sessionformat.SessionListPage{}, err
		}
		if ok && (latest.ID == "" || header.CreatedAt > latest.CreatedAt || header.CreatedAt == latest.CreatedAt && header.ID < latest.ID) {
			latest = header
		}
	}
	page := sessionformat.SessionListPage{Sessions: []sessionformat.Header{}}
	if latest.ID != "" {
		page.Sessions = append(page.Sessions, latest)
	}
	return page, nil
}

type hostSessionCandidate struct{ id, project string }

func hostSessionCandidates(ctx context.Context, logs, cursor string) ([]hostSessionCandidate, error) {
	projects, err := os.ReadDir(logs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var candidates []hostSessionCandidate
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !project.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(logs, project.Name()))
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if id := entry.Name(); entry.IsDir() && uuid.Validate(id) == nil && id > cursor {
				candidates = append(candidates, hostSessionCandidate{id, project.Name()})
			}
		}
	}
	return candidates, nil
}

func unarchivedHostSession(ctx context.Context, logs string, entry hostSessionCandidate) (sessionformat.Header, bool, error) {
	header, err := sessionformat.ReadProjectHeader(ctx, logs, entry.project, entry.id)
	if errors.Is(err, os.ErrNotExist) {
		return sessionformat.Header{}, false, nil
	}
	if err != nil || !topLevelSession(header) {
		return sessionformat.Header{}, false, err
	}
	store := sessionformat.FileStore{Root: logs, ProjectCWD: header.CWD}
	archived, err := archivedChat(store, entry.id)
	return header, !archived && err == nil, err
}
