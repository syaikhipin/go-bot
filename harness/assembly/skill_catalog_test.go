package assembly

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-bot/harness/agent"
	"go-bot/internal/testsupport/session"
)

type skillCatalogModel struct{ calls [][]agent.Message }

func (m *skillCatalogModel) Complete(_ context.Context, messages []agent.Message, _ []agent.ToolSpec) (session.AssistantMessage, error) {
	m.calls = append(m.calls, append([]agent.Message(nil), messages...))
	return session.AssistantMessage{Content: "done"}, nil
}

func TestNativeSkillCatalogRefreshAndColdResume(t *testing.T) {
	ctx := context.Background()
	builder := newContainerTestBuilder(t, t.TempDir())
	root := filepath.Join(builder.skillRoot, "skills")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(file, name, extra, body string) {
		t.Helper()
		document := "---\nname: " + name + "\ndescription: 'Use for <reports> & decks'\n" + extra + "---\n" + body
		if err := os.WriteFile(filepath.Join(root, file), []byte(document), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("report.md", "report", "user-invocable: false\n", "Original report")
	write("manual.md", "manual", "disable-model-invocation: true\n", "Only for explicit invocation")
	model := &skillCatalogModel{}
	factory := NativeFactory{
		Storage: &nativeMemoryStorage{}, Builder: nativeTestBuilder{builder}, Model: model,
		Provider: "test-provider", ModelID: "test-model", SystemPrompt: "Help with the user's task.",
		Limits: agent.Limits{MaxSteps: 4, MaxToolCalls: 8},
	}
	identity := NativeRuntimeIdentity{UserID: "alice", SessionID: "skill-catalog-test", Workspace: builder.mountRoot}
	live, err := factory.Activate(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	run := func(prompt string) []string {
		t.Helper()
		if _, err := live.Agent.RunTextTurn(ctx, prompt); err != nil {
			t.Fatal(err)
		}
		return visibleSkillCatalogs(model.calls[len(model.calls)-1])
	}
	catalogs := run("Please use /manual /manual and /report")
	if len(catalogs) != 1 || !strings.Contains(catalogs[0], "`report`: Use for &lt;reports&gt; &amp; decks") ||
		strings.Contains(catalogs[0], "`manual`") {
		t.Fatalf("initial catalog: %q", catalogs)
	}
	invoked := 0
	for _, message := range model.calls[0] {
		if strings.Contains(message.Content, "<skill_content name=\"report\">") {
			t.Fatal("user-disabled skill was injected")
		}
		if strings.Contains(message.Content, "Only for explicit invocation") {
			invoked++
		}
	}
	if invoked != 1 {
		t.Fatalf("explicit invocation count: %d", invoked)
	}
	write("report.md", "report", "user-invocable: false\n", "Edited report")
	if catalogs := run("Unchanged summary"); len(catalogs) != 1 {
		t.Fatalf("body edit republished catalog: %q", catalogs)
	}
	if err := os.Rename(root, root+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if catalogs := run("Unavailable root"); len(catalogs) != 2 || !strings.Contains(catalogs[1], "Skill discovery is incomplete") {
		t.Fatalf("partial replacement: %q", catalogs)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+".held", root); err != nil {
		t.Fatal(err)
	}
	if catalogs := run("Recovered root"); len(catalogs) != 3 || strings.Contains(catalogs[2], "Skill discovery is incomplete") {
		t.Fatalf("recovered replacement: %q", catalogs)
	}
	if err := os.Remove(filepath.Join(root, "report.md")); err != nil {
		t.Fatal(err)
	}
	if catalogs := run("Model-visible skill removed"); len(catalogs) != 4 || !strings.Contains(catalogs[3], "No skills are currently available") {
		t.Fatalf("empty replacement: %q", catalogs)
	}

	var sources []map[string]json.RawMessage
	for _, event := range live.Session.Events() {
		var row struct {
			Source map[string]json.RawMessage `json:"source"`
		}
		if event.Type == "user/message" && json.Unmarshal(event.Data, &row) == nil &&
			string(row.Source["kind"]) == `"skill-catalog"` {
			sources = append(sources, row.Source)
		}
	}
	if len(sources) != 4 || sources[0]["update"] != nil || sources[0]["incomplete"] != nil ||
		string(sources[1]["update"]) != "true" || string(sources[1]["incomplete"]) != "true" ||
		sources[2]["incomplete"] != nil || string(sources[3]["entries"]) != "[]" {
		t.Fatalf("durable catalog sources: %+v", sources)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	live, err = factory.Resume(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if catalogs := run("Cold resume with empty catalog"); len(catalogs) != 4 {
		t.Fatalf("resume duplicated unchanged catalog: %q", catalogs)
	}
}

func visibleSkillCatalogs(messages []agent.Message) []string {
	var catalogs []string
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, "<available_skills>") {
			catalogs = append(catalogs, message.Content)
		}
	}
	return catalogs
}
