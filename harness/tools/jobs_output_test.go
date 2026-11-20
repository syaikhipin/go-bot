package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-bot/harness/jobs"
	"go-bot/internal/modeldata"
)

func TestJobOutputFinalizerCapturesLimitBeforeConsumption(t *testing.T) {
	registry, err := jobs.New(t.Context(), 1, 256, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close(context.Background())
	snapshot, err := registry.Start("test", "bounded", func(context.Context, func([]byte)) (int, error) { return 0, nil })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"job_id": snapshot.ID})
	process := (JobOutput{Registry: registry}).PrepareOutput(raw)
	if process == nil {
		t.Fatal("PreExecute did not capture Job output policy")
	}
	suffix := "\njob_id=" + snapshot.ID + " status=completed exit_code=0 lossy=false next_cursor=999"
	result := process(modeldata.ToolResult{Content: strings.Repeat("output\n", 1000) + suffix})
	if len(result.Content) > 256 || !strings.HasSuffix(result.Content, suffix) {
		t.Fatalf("status was lost: %q", result.Content)
	}
}
