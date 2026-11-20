package executor

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"go-bot/harness/assembly"
	"go-bot/harness/jobs"
	"go-bot/harness/skills"
	"go-bot/harness/tools"
	"go-bot/internal/agentfs"
)

// Builder is installed only in the container worker binary. The Host shares one
// filesystem skill provider with its Sessions.
type Builder struct {
	RuntimeMode      string
	verifiedMode     string
	Skills           *skills.Registry
	WebFetcher       tools.WebFetcher
	WebSearcher      tools.WebSearcher
	ImageAttachments interface {
		tools.ImageAttachmentWriter
		tools.ImageReferenceReader
	}
	ImageInputEnabled bool
	ImageGenerator    tools.ImageGenerator
}

// NewVerifiedBuilder performs the Host-wide mount and file-semantics check
// once at startup. Every Session built from this value uses the same already
// verified container environment; a separately constructed Builder still
// checks its runtime before creating a Shell executor.
func NewVerifiedBuilder(mode string) (Builder, error) {
	if err := VerifyHostRuntime(mode); err != nil {
		return Builder{}, err
	}
	registry := &skills.Registry{DSHHome: skills.UserDSHHome, AgentsHome: skills.UserAgentsHome}
	return Builder{RuntimeMode: mode, verifiedMode: mode, Skills: registry}, nil
}

func (b Builder) BuildNative(ctx context.Context, identity assembly.NativeRuntimeIdentity, reason string) (assembly.Resources, error) {
	if identity.UserID == "" || identity.SessionID == "" || !filepath.IsAbs(identity.Workspace) ||
		(reason != "activation" && reason != "resume") {
		return assembly.Resources{}, errors.New("invalid native container Assembly identity")
	}
	cwd := identity.CWD
	if cwd == "" {
		cwd = identity.Workspace
	}
	if !filepath.IsAbs(cwd) || len(cwd) > 4096 {
		return assembly.Resources{}, errors.New("native container Agent cwd must be absolute")
	}
	return b.build(ctx, identity.UserID, cwd)
}

func (b Builder) build(ctx context.Context, userID, cwd string) (assembly.Resources, error) {
	if err := ctx.Err(); err != nil {
		return assembly.Resources{}, err
	}
	if b.verifiedMode != b.RuntimeMode {
		if err := VerifyHostRuntime(b.RuntimeMode); err != nil {
			return assembly.Resources{}, err
		}
	}
	jobRegistry, err := jobs.New(context.Background(), jobs.DefaultMaxConcurrent, jobs.DefaultRetainBytes, nil)
	if err != nil {
		return assembly.Resources{}, err
	}
	browser, err := newBrowserSession()
	if err != nil {
		_ = jobRegistry.Close(ctx)
		return assembly.Resources{}, err
	}
	resources := assembly.Resources{
		Files:           agentfs.ContainerView{DefaultWorkdir: cwd},
		ConfineMutation: confineMutation,
		ProbeSandbox:    probeSandbox,
		Shell:           Executor{verified: true, environment: browser.environment()},
		DefaultWorkdir:  cwd,
		Jobs:            jobRegistry,
		Skills:          b.Skills,
		Close: func() error {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			jobsErr := jobRegistry.Close(stopCtx)
			return errors.Join(jobsErr, browser.close())
		},
	}
	if b.WebFetcher != nil {
		resources.ExtraTools = append(resources.ExtraTools, tools.WebFetch{Fetcher: b.WebFetcher, SpillWorkdir: cwd})
	}
	if b.WebSearcher != nil {
		resources.ExtraTools = append(resources.ExtraTools, tools.WebSearch{Searcher: b.WebSearcher, SpillWorkdir: cwd})
	}
	resources.ExtraTools = append(resources.ExtraTools,
		tools.RipgrepSearch{Name: "glob", Workdir: cwd},
		tools.RipgrepSearch{Name: "grep", Workdir: cwd},
	)
	if b.ImageAttachments != nil {
		resources.ExtraTools = append(resources.ExtraTools, tools.ReadImage{View: resources.Files, Attachments: b.ImageAttachments, ImageInputEnabled: b.ImageInputEnabled})
	}
	if b.ImageGenerator != nil {
		resources.ExtraTools = append(resources.ExtraTools, tools.GenerateImage{
			Generator: b.ImageGenerator, Attachments: b.ImageAttachments, References: b.ImageAttachments, UserID: userID,
		})
	}
	return resources, nil
}
