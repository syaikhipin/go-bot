package assembly

import (
	"go-bot/harness/agent"
	"go-bot/harness/skills"
)

// NativeComposition selects capabilities at assembly time. It does not own
// sessions, run turns, wrap tool execution, or manage Cordis plugin lifetimes.
// Nil selectors preserve the ordinary Host composition.
type NativeComposition struct {
	ID           string
	SystemPrompt string
	SystemSuffix string
	AllowTool    func(agent.ToolSpec) bool
	AllowSkill   func(string) bool
}

func (c *NativeComposition) tools(input []agent.Tool) []agent.Tool {
	if c == nil || c.AllowTool == nil {
		return input
	}
	output := make([]agent.Tool, 0, len(input))
	for _, tool := range input {
		if tool != nil && c.AllowTool(tool.Spec()) {
			output = append(output, tool)
		}
	}
	return output
}

func (c *NativeComposition) skills(registry *skills.Registry) *skills.Registry {
	if c == nil || c.AllowSkill == nil {
		return registry
	}
	// An Agent view must never mutate the shared Host registry. Snapshot,
	// skill loading and explicit invocation all consult this same view.
	view := *registry
	prior := registry.Visible
	view.Visible = func(lookup skills.Lookup, name string) bool {
		return (prior == nil || prior(lookup, name)) && c.AllowSkill(name)
	}
	return &view
}
