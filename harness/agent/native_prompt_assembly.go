package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type PromptAssemblyInput struct {
	SessionID      string                  `json:"sessionId"`
	CWD            string                  `json:"cwd"`
	SystemPrompt   string                  `json:"systemPrompt"`
	Tools          []ToolSpec              `json:"tools"`
	ModelSelection ModelRoute              `json:"modelSelection"`
	SandboxPolicy  *ExecutionSandboxPolicy `json:"sandboxPolicy,omitempty"`
}
type PromptAssembly struct {
	SystemPrompt string                  `json:"systemPrompt"`
	Contexts     []RuntimeContextSection `json:"contexts"`
	Tools        []ToolSpec              `json:"tools"`
}
type PromptAssemblySource func(context.Context, PromptAssemblyInput) (PromptAssembly, error)

func (a *NativeLoop) SetPromptAssembler(source PromptAssemblySource) { a.promptAssembler = source }

func (a *NativeLoop) SetToolStepFinalizer(finalize func(context.Context, string) error) {
	a.finishToolStep = finalize
}
func (a *NativeLoop) releaseToolStep(ctx context.Context) error {
	if a.finishToolStep == nil {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return a.finishToolStep(cleanup, a.session.Header().ID)
}

// A prompt provider may declare only tools backed by this step's real tool
// table. The original handler, validation, approval and execution metadata stay
// pinned. Assemble everything before replacing the visible table.
func (a *NativeLoop) assemblePrompt(ctx context.Context, prompt string) (string, error) {
	if a.promptAssembler == nil {
		return prompt, nil
	}
	input := PromptAssemblyInput{SessionID: a.session.Header().ID, CWD: a.session.Header().CWD, SystemPrompt: prompt, Tools: slices.Clone(a.toolSpecs), ModelSelection: ModelRoute{Provider: a.provider, Model: a.modelID, ReasoningEffort: a.reasoningEffort}}
	input.SandboxPolicy = a.executionSandboxPolicy()
	assembly, err := a.promptAssembler(ctx, input)
	if err != nil {
		return "", err
	}
	if len(assembly.SystemPrompt) > 1<<20 || len(assembly.Contexts) > 1024 {
		return "", errors.New("plugin prompt assembly exceeds supported limits")
	}
	tools := map[string]Tool{}
	schemas := map[string]*jsonschema.Schema{}
	safe := map[string]bool{}
	for i, spec := range assembly.Tools {
		tool := a.tools[spec.Name]
		if tool == nil || tools[spec.Name] != nil {
			return "", fmt.Errorf("prompt assembly names unknown or duplicate tool %q", spec.Name)
		}
		schema, err := compileToolParameters(spec.Parameters)
		if err != nil {
			return "", fmt.Errorf("prompt assembly tool %q: %w", spec.Name, err)
		}
		tools[spec.Name], schemas[spec.Name], safe[spec.Name] = tool, schema, a.parallelSafe[spec.Name]
		assembly.Tools[i].ParallelSafe = safe[spec.Name]
		assembly.Tools[i].Timeout = tool.Spec().Timeout
	}
	size := 0
	for _, section := range assembly.Contexts {
		size += len(section.Name) + len(section.Text)
	}
	if size > 256<<10 {
		return "", errors.New("plugin prompt context exceeds 256 KiB")
	}
	a.tools, a.toolSchemas, a.parallelSafe, a.toolSpecs = tools, schemas, safe, assembly.Tools
	a.assembledContexts = slices.Clone(assembly.Contexts)
	return assembly.SystemPrompt, nil
}
