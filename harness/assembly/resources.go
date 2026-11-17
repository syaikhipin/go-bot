package assembly

import (
	"context"
	"go-bot/harness/agent"
	"go-bot/harness/jobs"
	"go-bot/harness/skills"
	"go-bot/harness/tools"
)

// Resources are the native Session's container-local tools and Skill catalog.
// The user Host creates them for each top-level or child Session.
type Resources struct {
	Files           tools.FileView
	ConfineMutation func(context.Context) context.Context
	ProbeSandbox    func(context.Context) error
	Shell           tools.CommandExecutor
	DefaultWorkdir  string
	Jobs            *jobs.Registry
	Skills          *skills.Registry
	ExtraTools      []agent.Tool
	Close           func() error
}

func (r Resources) close() error {
	if r.Close != nil {
		return r.Close()
	}
	return nil
}

func baseTools(resource Resources) ([]agent.Tool, error) {
	toolset, err := tools.NewObservedFileTools(resource.Files)
	if err != nil {
		return nil, err
	}
	toolset = append(toolset, tools.Shell{Executor: resource.Shell, Jobs: resource.Jobs, DefaultWorkdir: resource.DefaultWorkdir})
	if resource.Jobs != nil {
		toolset = append(toolset, tools.JobOutput{Registry: resource.Jobs}, tools.JobList{Registry: resource.Jobs}, tools.JobKill{Registry: resource.Jobs})
	}
	return toolset, nil
}
