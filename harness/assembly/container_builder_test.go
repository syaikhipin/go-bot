package assembly

import (
	"context"
	"errors"
	"testing"

	"go-harness/harness/agent"
	"go-harness/harness/skills"
	"go-harness/harness/tools"
	"go-harness/internal/agentfs"
	"go-harness/internal/testsupport/session"
)

// containerTestBuilder exercises the container Assembly contract without
// starting Docker. Integration tests exercise the real worker and image.
type containerTestBuilder struct {
	mountRoot string
	skillRoot string
}

func newContainerTestBuilder(t *testing.T, mountRoot string) containerTestBuilder {
	t.Helper()
	return containerTestBuilder{mountRoot: mountRoot, skillRoot: t.TempDir()}
}

func (b containerTestBuilder) Build(ctx context.Context, header session.Header, _ string) (Resources, error) {
	if header.Mode != "container" {
		return Resources{}, errors.New("test builder requires container mode")
	}
	if err := ctx.Err(); err != nil {
		return Resources{}, err
	}
	workspace, err := session.EnsureSessionWorkspaceOnly(b.mountRoot, header.UserID, header.EffectiveWorkspaceOwnerID())
	if err != nil {
		return Resources{}, err
	}
	return Resources{
		Files:  agentfs.ContainerView{DefaultWorkdir: workspace},
		Shell:  unusedContainerTestShell{},
		Skills: &skills.Registry{DSHHome: b.skillRoot},
	}, nil
}

type unusedContainerTestShell struct{}

func (unusedContainerTestShell) Run(context.Context, string, string, int) (tools.ShellResult, error) {
	return tools.ShellResult{}, errors.New("test shell was unexpectedly invoked")
}

type containerTestBuilderWithTools struct {
	containerTestBuilder
	extra func(session.Header, tools.FileView) []agent.Tool
}

func (b containerTestBuilderWithTools) Build(ctx context.Context, header session.Header, reason string) (Resources, error) {
	resource, err := b.containerTestBuilder.Build(ctx, header, reason)
	if err != nil {
		return Resources{}, err
	}
	if b.extra != nil {
		resource.ExtraTools = b.extra(header, resource.Files)
	}
	return resource, nil
}
