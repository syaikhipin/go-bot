package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func document(name, fields, body string) string {
	return "---\nname: " + name + "\ndescription: For " + name + "\n" + fields + "---\n" + body
}

func TestDiscoveryUsesProjectAndUserPrecedenceAndFreshBody(t *testing.T) {
	ctx := context.Background()
	workspace, outside := t.TempDir(), t.TempDir()
	repo := filepath.Join(workspace, "repo")
	cwd := filepath.Join(repo, "nested")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, ".git"), "gitdir: /tmp/worktree")
	dsh, agents := t.TempDir(), t.TempDir()
	r := Registry{DSHHome: dsh, AgentsHome: agents}
	path := filepath.Join(repo, ".dsh/skills/local/SKILL.md")
	write(t, path, document("shared", "disable-model-invocation: YES\nwhenToUse: Explicit only\nmetadata: { version: 1 }\n", "First"))
	write(t, filepath.Join(repo, ".agents/skills/shared.md"), document("shared", "", "Project shared"))
	write(t, filepath.Join(dsh, "skills/elsewhere.md"), document("shared", "user-invocable: 0\n", "User DSH"))
	write(t, filepath.Join(agents, "skills/shared.md"), document("shared", "", "User Agents"))
	write(t, filepath.Join(agents, "skills/second.md"), document("second", "", "Second"))
	write(t, filepath.Join(dsh, "skills/.system/SKILL.md"), document("system", "", "Hidden"))
	write(t, filepath.Join(dsh, "skills/legacy.md"), document("legacy", "userInvocable: true\n", "Invalid"))
	write(t, filepath.Join(dsh, "skills/invalid.md"), document("invalid", "user-invocable: []\n", "Invalid"))
	write(t, filepath.Join(outside, ".dsh/skills/leak.md"), document("leak", "", "Outside project"))
	if err := os.Symlink(filepath.Join(outside, ".dsh/skills/leak.md"), filepath.Join(dsh, "skills/leak.md")); err != nil {
		t.Fatal(err)
	}

	project := Lookup{CWD: cwd, Workspace: workspace}
	snapshot, err := r.Snapshot(ctx, project)
	if err != nil || !snapshot.Complete || len(snapshot.Entries) != 2 ||
		snapshot.Entries[0].Name != "second" || snapshot.Entries[1].Name != "shared" {
		t.Fatalf("discovery: %+v, %v", snapshot, err)
	}
	winner := snapshot.Entries[1]
	if winner.ModelInvocable || !winner.UserInvocable || winner.WhenToUse != "Explicit only" || winner.Metadata["version"] != 1 {
		t.Fatalf("project policy: %+v", winner)
	}
	write(t, path, document("shared", "disable-model-invocation: yes\n", "\nEdited body\n\n"))
	loaded, found, err := winner.Load(ctx)
	if err != nil || !found || loaded.Value.Content != "Edited body" || loaded.Value.Provider != "filesystem" ||
		!strings.HasSuffix(loaded.Value.ResourceBase.Path, "/repo/.dsh/skills/local") {
		t.Fatalf("latest skill: %+v, %t, %v", loaded, found, err)
	}
	write(t, path, document("renamed", "", "Changed identity"))
	if _, found, err := winner.Load(ctx); err != nil || found {
		t.Fatalf("old locator still loads after rename: %t, %v", found, err)
	}
	for _, tt := range []struct {
		lookup Lookup
		body   string
		user   bool
	}{
		{project, "Project shared", true},
		{Lookup{CWD: outside, Workspace: workspace}, "User DSH", false},
	} {
		entry, found, err := r.Find(ctx, tt.lookup, "shared")
		if err != nil || !found {
			t.Fatalf("lookup: %t, %v", found, err)
		}
		loaded, found, err := entry.Load(ctx)
		if err != nil || !found || loaded.Value.Content != tt.body || loaded.UserInvocable != tt.user ||
			filepath.Base(loaded.Value.ResourceBase.Path) != "skills" {
			t.Fatalf("fallback: %+v, %t, %v", loaded, found, err)
		}
	}
}

func TestDiscoveryPublishesPartialRootsAndFallsBack(t *testing.T) {
	ctx := context.Background()
	dsh, agents := t.TempDir(), t.TempDir()
	foreign := t.TempDir()
	write(t, filepath.Join(foreign, "shared.md"), document("shared", "", "Excluded"))
	if err := os.Symlink(foreign, filepath.Join(dsh, "skills")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(agents, "skills/shared.md"), document("shared", "", "Fallback"))
	r := Registry{DSHHome: dsh, AgentsHome: agents}
	snapshot, err := r.Snapshot(ctx, Lookup{})
	if err != nil || snapshot.Complete || len(snapshot.Entries) != 1 {
		t.Fatalf("partial discovery: %+v, %v", snapshot, err)
	}
	loaded, found, err := snapshot.Entries[0].Load(ctx)
	if err != nil || !found || loaded.Value.Content != "Fallback" {
		t.Fatalf("partial winner: %+v, %t, %v", loaded, found, err)
	}
	if err := os.Remove(filepath.Join(dsh, "skills")); err != nil {
		t.Fatal(err)
	}
	snapshot, err = r.Snapshot(ctx, Lookup{})
	if err != nil || !snapshot.Complete || len(snapshot.Entries) != 1 {
		t.Fatalf("recovered discovery: %+v, %v", snapshot, err)
	}
}

func TestBootstrapPreservesExistingEditsAndDeletion(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "user override"}[existing], func(t *testing.T) {
			ctx := context.Background()
			data := t.TempDir()
			seed := t.TempDir()
			write(t, filepath.Join(seed, "officecli/SKILL.md"), document("officecli", "", "Official body"))
			write(t, filepath.Join(seed, "officecli-docx/SKILL.md"), document("officecli-docx", "", "Word"))
			write(t, filepath.Join(seed, "officecli-docx/reference/template.md"), "Template")
			browser := filepath.Join(data, ".dsh/skills/agent-browser/SKILL.md")
			write(t, filepath.Join(seed, "agent-browser/SKILL.md"), document("agent-browser", "", "Browser"))
			destination := filepath.Join(data, ".dsh/skills/officecli/SKILL.md")
			expected := "Official body"
			if existing {
				destination = filepath.Join(data, ".agents/skills/my-office.md")
				expected = "User body"
				write(t, destination, document("officecli", "", expected))
			}
			if err := Bootstrap(ctx, data, seed); err != nil {
				t.Fatal(err)
			}
			if asset, err := os.ReadFile(filepath.Join(data, ".dsh/skills/officecli-docx/reference/template.md")); err != nil || string(asset) != "Template" {
				t.Fatalf("specialized bundle asset: %s, %v", asset, err)
			}
			if content, err := os.ReadFile(browser); err != nil || !strings.Contains(string(content), "Browser") {
				t.Fatalf("browser skill: %s, %v", content, err)
			}
			if content, err := os.ReadFile(destination); err != nil || !strings.Contains(string(content), expected) {
				t.Fatalf("installed skill: %s, %v", content, err)
			}
			if existing {
				if _, err := os.Stat(filepath.Join(data, ".dsh/skills/officecli/SKILL.md")); !os.IsNotExist(err) {
					t.Fatalf("existing shared skill was shadowed: %v", err)
				}
			}
			write(t, destination, document("officecli", "", "Edited body"))
			if err := Bootstrap(ctx, data, seed); err != nil {
				t.Fatal(err)
			}
			if content, err := os.ReadFile(destination); err != nil || !strings.Contains(string(content), "Edited body") {
				t.Fatalf("edited skill: %s, %v", content, err)
			}
			if err := os.Remove(destination); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(browser); err != nil {
				t.Fatal(err)
			}
			if err := Bootstrap(ctx, data, seed); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatalf("removed skill reappeared: %v", err)
			}
			if _, err := os.Stat(browser); !os.IsNotExist(err) {
				t.Fatalf("removed browser skill reappeared: %v", err)
			}
		})
	}
}

func TestBootstrapAddsBrowserToPreviouslySeededData(t *testing.T) {
	data, seed := t.TempDir(), t.TempDir()
	write(t, filepath.Join(data, legacyOfficeMarker), "")
	write(t, filepath.Join(seed, "officecli/SKILL.md"), document("officecli", "", "Deleted by user"))
	write(t, filepath.Join(seed, "agent-browser/SKILL.md"), document("agent-browser", "", "Browser"))
	if err := Bootstrap(context.Background(), data, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, ".dsh/skills/officecli/SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("deleted OfficeCLI skill reappeared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, ".dsh/skills/agent-browser/SKILL.md")); err != nil {
		t.Fatalf("browser skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, seedMarker)); err != nil {
		t.Fatalf("skill initialization marker missing: %v", err)
	}
}
