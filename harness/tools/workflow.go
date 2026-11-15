package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go-harness/harness/agent"
	"go-harness/harness/jobs"
	"go-harness/harness/session"
	"go-harness/harness/workflow"

	"github.com/google/uuid"
)

// WorkflowCoordinator belongs to the user Host. Every member is a child
// Session of the calling Agent in that same Host, never a new container.
type WorkflowCoordinator interface {
	RunWorkflowChildObserved(context.Context, string, string, workflow.AgentOptions, func(string)) (workflow.AgentResult, error)
}

type NativeWorkflow struct {
	Coordinator WorkflowCoordinator
	ParentID    string
	Session     *sessionformat.LiveSession
	Jobs        *jobs.Registry
}

type workflowCall struct {
	Script string `json:"script"`
	Meta   struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		WhenToUse   string `json:"whenToUse,omitempty"`
		Phases      []struct {
			Title    string `json:"title"`
			Detail   string `json:"detail,omitempty"`
			Provider string `json:"provider,omitempty"`
			Model    string `json:"model,omitempty"`
		} `json:"phases,omitempty"`
	} `json:"meta"`
	MetaRaw         map[string]any `json:"-"`
	Args            map[string]any `json:"args,omitempty"`
	RunInBackground bool           `json:"run_in_background,omitempty"`
}

func decodeWorkflowCall(raw json.RawMessage) (workflowCall, error) {
	var args workflowCall
	var fields map[string]json.RawMessage
	if err := decodeOneToolObject(raw, &fields); err != nil {
		return args, err
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, err
	}
	if meta, present := fields["meta"]; present {
		if err := json.Unmarshal(meta, &args.MetaRaw); err != nil {
			return args, err
		}
	}
	return args, nil
}

func (t NativeWorkflow) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "workflow",
		Description: "Run a JavaScript workflow for an explicit workflow request or large multi-agent orchestration. Use agent(prompt, opts?) to delegate, parallel(thunks) for a barrier, pipeline(items, ...stages) for per-item stages, phase(title), log(message), args, and meta. Return plain JSON data. For one or two delegations, prefer subagent. Child Agents always inherit model.main; do not supply provider or model options. The script has no filesystem, network, timer, or Node APIs; child Agents do the work. A background run returns a managed Job ID.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"script":{"type":"string"},"meta":{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"whenToUse":{"type":"string"},"phases":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"},"provider":{"type":"string"},"model":{"type":"string"}},"required":["title"],"additionalProperties":true}}},"required":["name","description"],"additionalProperties":true},"args":{"type":"object","additionalProperties":true},"run_in_background":{"type":"boolean"}},"required":["script","meta"]}`),
	}
}

func (t NativeWorkflow) parse(raw json.RawMessage) (workflowCall, error) {
	var args workflowCall
	if t.Coordinator == nil || t.ParentID == "" || t.Session == nil {
		return args, errors.New("workflow Host coordinator is unavailable")
	}
	decoded, err := decodeWorkflowCall(raw)
	if err != nil {
		return args, err
	}
	args = decoded
	if _, err := workflow.Compile(args.Script); err != nil {
		return args, err
	}
	if args.Meta.Name == "" || args.Meta.Description == "" {
		return args, errors.New("workflow meta requires a name and description")
	}
	for _, phase := range args.Meta.Phases {
		if phase.Title == "" {
			return args, errors.New("workflow meta phase requires a title")
		}
	}
	if args.RunInBackground && t.Jobs == nil {
		return args, errors.New("background workflow jobs are unavailable")
	}
	return args, nil
}

func (t NativeWorkflow) Validate(raw json.RawMessage) error {
	_, err := t.parse(raw)
	return err
}

func (t NativeWorkflow) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := t.parse(raw)
	if err != nil {
		return "", err
	}
	runID := uuid.NewString()
	recorder := &workflowEventRecorder{session: t.Session, active: true}
	if args.RunInBackground {
		startGate := make(chan struct{})
		job, err := t.Jobs.Start("workflow", args.Meta.Name, func(jobCtx context.Context, emit func([]byte)) (int, error) {
			select {
			case <-startGate:
			case <-jobCtx.Done():
				return 1, jobCtx.Err()
			}
			result, runErr := t.run(jobCtx, runID, args, recorder)
			if runErr != nil {
				return 1, runErr
			}
			emit([]byte(renderWorkflowResult(args.Meta.Name, result)))
			return 0, nil
		})
		if err != nil {
			return "", err
		}
		recorder.append("tool-workflow/run-start", map[string]any{"runId": runID, "name": args.Meta.Name})
		close(startGate)
		if _, err := t.Jobs.MarkVisible(job.ID); err != nil {
			_, _ = t.Jobs.Kill(job.ID)
			return "", err
		}
		return fmt.Sprintf("workflow %q started in the background as job %s (run %s). Read the result with job_output; stop it with job_kill.", args.Meta.Name, job.ID, runID), nil
	}
	recorder.append("tool-workflow/run-start", map[string]any{"runId": runID, "name": args.Meta.Name})
	result, err := t.run(ctx, runID, args, recorder)
	if err != nil {
		return "", err
	}
	return renderWorkflowResult(args.Meta.Name, result), nil
}

func (t NativeWorkflow) run(ctx context.Context, runID string, args workflowCall, recorder *workflowEventRecorder) (workflow.Result, error) {
	result, err := workflow.ExecuteRunObserved(ctx, args.Script, args.Args, args.MetaRaw, workflow.Limits{}, workflow.Observer{
		AgentStart: func(info workflow.AgentLifecycle) {
			data := map[string]any{"runId": runID, "seq": info.Seq, "label": info.Label, "childId": info.ChildID}
			if info.Phase != "" {
				data["phase"] = info.Phase
			}
			recorder.append("tool-workflow/agent-start", data)
		},
		AgentEnd: func(info workflow.AgentLifecycle) {
			recorder.append("tool-workflow/agent-end", map[string]any{"runId": runID, "seq": info.Seq, "outcome": info.Outcome})
		},
	}, func(childCtx context.Context, prompt string, options workflow.AgentOptions, started func(string)) (workflow.AgentResult, error) {
		return t.Coordinator.RunWorkflowChildObserved(childCtx, t.ParentID, prompt, options, started)
	})
	reason := "completed"
	if err != nil {
		reason = "error"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = "cancelled"
		}
	}
	recorder.append("tool-workflow/run-end", map[string]any{"runId": runID, "stopReason": reason})
	return result, err
}

func renderWorkflowResult(name string, result workflow.Result) string {
	encoded, err := json.MarshalIndent(result.Value, "", "  ")
	if err != nil {
		encoded = []byte("null")
	}
	const maxChars = 50_000
	characters := []rune(string(encoded))
	text := string(encoded)
	if len(characters) > maxChars {
		text = string(characters[:maxChars]) + fmt.Sprintf("\n… [truncated: %d more characters]", len(characters)-maxChars)
	}
	plural := "s"
	if result.AgentsStarted == 1 {
		plural = ""
	}
	return fmt.Sprintf("workflow %q completed (%d agent%s).\nReturn value:\n%s", name, result.AgentsStarted, plural, text)
}

// DeepSeek's four tool-workflow events are log-only projections. Recording
// failure disables this run's projection but does not change the script or
// child result. The Session writer itself remains responsible for surfacing
// an uncertain v4 checkpoint to its owning Agent.
type workflowEventRecorder struct {
	mu      sync.Mutex
	session *sessionformat.LiveSession
	active  bool
}

func (r *workflowEventRecorder) append(kind string, data map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return
	}
	encoded, err := json.Marshal(data)
	if err == nil {
		_, err = r.session.Append(sessionformat.EventInput{Type: kind, Data: encoded})
	}
	if err == nil {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = r.session.Flush(flushCtx)
		cancel()
	}
	if err != nil {
		r.active = false
	}
}
