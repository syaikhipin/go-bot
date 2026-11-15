package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-harness/harness/skills"
)

func TestSkillStructuredResultAndPolicy(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "skills", "a & <b>")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "SKILL.md")
	write := func(policy string) {
		t.Helper()
		content := "---\nname: report\ndescription: Make reports\n" + policy + "---\n\nUse **this** format.\n"
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("user-invocable: false\n")
	tool := Skill{Registry: &skills.Registry{DSHHome: home}, Lookup: func() skills.Lookup { return skills.Lookup{} }}
	ctx := context.Background()
	raw := json.RawMessage(`{"name":"report"}`)
	value, err := tool.ExecuteValue(ctx, raw)
	if err != nil || value.Name != "report" || value.Provider != "filesystem" || value.Content != "Use **this** format." ||
		value.ResourceBase.Kind != "directory" || !strings.HasSuffix(value.ResourceBase.Path, "/a & <b>") {
		t.Fatalf("value: %+v, %v", value, err)
	}
	rendered, meta, err := tool.ExecutePresented(ctx, raw)
	if err != nil || meta != nil || !strings.Contains(rendered, "a &amp; &lt;b&gt;") ||
		!strings.Contains(rendered, "<skill_instructions>\nUse **this** format.\n</skill_instructions>") {
		t.Fatalf("rendered: %q, %s, %v", rendered, meta, err)
	}
	write("disable-model-invocation: true\n")
	for _, tt := range []struct{ args, errorText string }{
		{`{"name":"Invalid!"}`, `invalid skill name "Invalid!"`},
		{`{"name":"missing"}`, `skill "missing" is unknown or no longer available`},
		{`{"name":"report"}`, `skill "report" is not available for model invocation`},
	} {
		_, err := tool.Execute(ctx, json.RawMessage(tt.args))
		if err == nil || err.Error() != tt.errorText {
			t.Fatalf("%s: %v", tt.args, err)
		}
	}
}
