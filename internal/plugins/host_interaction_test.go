package plugins

import (
	"context"
	"strings"
	"testing"
	"time"

	"go-bot/harness/agent"
	"go-bot/harness/interaction"
	sessionformat "go-bot/harness/session"
	"go-bot/internal/modeldata"
	"go-bot/plugin"
)

func TestHostBridgeToolBusinessApprovalUsesGoOutcome(t *testing.T) {
	r := bridgeFixture(t, `export const inject=['host'];export async function apply(ctx){await ctx.host.tools.register({name:'send',description:'Send with approval',inputSchema:{type:'object'},async execute(args,call){const decision=await ctx.host.approvals.request(call,{reason:'Send one message'});return{content:[{type:'text',text:decision==='allowed'?'sent':'skipped:'+decision}]}}})}`)
	if _, err := r.Apply(t.Context(), []hostPluginInstance{{ID: "mail", Package: "fixture", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"allowed", "rejected", "cancelled", "unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			approve := func(ctx context.Context, ask agent.ToolPolicyAsk) error {
				if _, deadline := ctx.Deadline(); deadline {
					t.Error("ordinary approval inherited a short RPC deadline")
				}
				if ask.Reason != "Send one message" {
					t.Error(ask)
				}
				if outcome != "allowed" {
					return &plugin.Error{Code: "APPROVAL_" + strings.ToUpper(outcome), Message: outcome}
				}
				return nil
			}
			result := bridgeRunTool(t, r, "mail__send", approve)
			decoded, err := agent.DecodeToolPipelineResult(result)
			if err != nil || result.IsError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			want := "sent"
			if outcome != "allowed" {
				want = "skipped:" + outcome
			}
			if decoded.Content != want {
				t.Fatalf("result=%+v", decoded)
			}
		})
	}
}

type bridgeJournal struct{}

func (bridgeJournal) Append(context.Context, []sessionformat.Event) error { return nil }
func (bridgeJournal) Close() error                                        { return nil }

type bridgeModel func(context.Context, []agent.Message, []agent.ToolSpec) (modeldata.AssistantMessage, error)

func (f bridgeModel) Complete(ctx context.Context, m []agent.Message, t []agent.ToolSpec) (modeldata.AssistantMessage, error) {
	return f(ctx, m, t)
}

func TestHostBridgeQuestionWaitsAndAnswersWithinRealGoTurn(t *testing.T) {
	r := bridgeFixture(t, `export const inject=['host'];export async function apply(ctx){await ctx.host.tools.register({name:'choose',description:'Ask about delivery',inputSchema:{type:'object'},async execute(args,call){const result=await ctx.host.questions.ask(call,{questions:[{id:'delivery',question:'Choose delivery',options:[{label:'Email'},{label:'Download'}]}]});return{content:[{type:'text',text:JSON.stringify(result)}]}}})}`)
	if _, err := r.Apply(t.Context(), []hostPluginInstance{{ID: "mail", Package: "fixture", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	live, err := sessionformat.NewLiveSession(sessionformat.Header{Type: "session", Version: 4, ID: "question-session", CreatedAt: 1, CWD: r.config.Workspace}, bridgeJournal{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	questions := interaction.NewNativeQuestions()
	questions.Enable()
	defer questions.Close()
	calls := 0
	model := bridgeModel(func(_ context.Context, messages []agent.Message, _ []agent.ToolSpec) (modeldata.AssistantMessage, error) {
		calls++
		if calls == 1 {
			return modeldata.AssistantMessage{ToolCalls: []modeldata.ToolRequest{{ID: "choose-call", Name: "mail__choose", Arguments: `{}`}}}, nil
		}
		found := false
		for _, message := range messages {
			if message.Role == "tool" && strings.Contains(message.Content, "Email") {
				found = true
			}
		}
		if !found {
			t.Error("the Go model context did not receive the question answer")
		}
		return modeldata.AssistantMessage{Content: "done"}, nil
	})
	loop, err := agent.NewNativeLoop(live, model, "test", "test", "core prompt", nil, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	loop.SetToolQuestions(questions)
	loop.SetToolSource(r.Tools)
	loop.SetToolInvocationSource(func(context.Context, string) (agent.ToolInvocationFactory, error) { return hostToolInvocation, nil })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err = loop.AcceptText(ctx, "Ask a question"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := loop.RunNext(ctx); done <- err }()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	answered := false
	for !answered {
		select {
		case err := <-done:
			t.Fatalf("turn ended before the user answered: %v", err)
		case <-ctx.Done():
			t.Fatal("question was not published")
		case <-tick.C:
			pending := questions.Pending()
			if len(pending) == 0 {
				continue
			}
			if err := questions.Answer(ctx, pending[0].CallID, interaction.Answer{Answers: []interaction.AnswerItem{{ID: "delivery", Selected: []string{"Email"}}}}); err != nil {
				t.Fatal(err)
			}
			answered = true
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("turn did not resume after the answer")
	}
	if calls != 2 || len(questions.Pending()) != 0 {
		t.Fatalf("calls=%d pending=%v", calls, questions.Pending())
	}
	if err := sessionformat.ValidateV4Relationships(live.DurableEvents()); err != nil {
		t.Fatal(err)
	}
}
