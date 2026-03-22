package agent

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func newHookTestLoop(
	t *testing.T,
	provider providers.LLMProvider,
) (*AgentLoop, *AgentInstance, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "agent-hooks-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	return al, agent, func() {
		al.Close()
		_ = os.RemoveAll(tmpDir)
	}
}

func TestHookManager_SortsInProcessBeforeProcess(t *testing.T) {
	hm := NewHookManager(nil)
	defer hm.Close()

	if err := hm.Mount(HookRegistration{
		Name:     "process",
		Priority: -10,
		Source:   HookSourceProcess,
		Hook:     struct{}{},
	}); err != nil {
		t.Fatalf("mount process hook: %v", err)
	}
	if err := hm.Mount(HookRegistration{
		Name:     "in-process",
		Priority: 100,
		Source:   HookSourceInProcess,
		Hook:     struct{}{},
	}); err != nil {
		t.Fatalf("mount in-process hook: %v", err)
	}

	ordered := hm.snapshotHooks()
	if len(ordered) != 2 {
		t.Fatalf("expected 2 hooks, got %d", len(ordered))
	}
	if ordered[0].Name != "in-process" {
		t.Fatalf("expected in-process hook first, got %q", ordered[0].Name)
	}
	if ordered[1].Name != "process" {
		t.Fatalf("expected process hook second, got %q", ordered[1].Name)
	}
}

type llmHookTestProvider struct {
	mu        sync.Mutex
	lastModel string
	lastMsgN  int
}

func (p *llmHookTestProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.lastModel = model
	p.lastMsgN = len(messages)
	p.mu.Unlock()

	return &providers.LLMResponse{
		Content: "provider content",
	}, nil
}

func (p *llmHookTestProvider) GetDefaultModel() string {
	return "llm-hook-provider"
}

type llmObserverHook struct {
	eventCh chan Event
}

func (h *llmObserverHook) OnEvent(ctx context.Context, evt Event) error {
	if evt.Kind == EventKindTurnEnd {
		select {
		case h.eventCh <- evt:
		default:
		}
	}
	return nil
}

func (h *llmObserverHook) BeforeLLM(
	ctx context.Context,
	req *LLMHookRequest,
) (*LLMHookRequest, HookDecision, error) {
	next := req.Clone()
	next.Model = "hook-model"
	return next, HookDecision{Action: HookActionModify}, nil
}

func (h *llmObserverHook) AfterLLM(
	ctx context.Context,
	resp *LLMHookResponse,
) (*LLMHookResponse, HookDecision, error) {
	next := resp.Clone()
	next.Response.Content = "hooked content"
	return next, HookDecision{Action: HookActionModify}, nil
}

func TestAgentLoop_Hooks_ObserverAndLLMInterceptor(t *testing.T) {
	provider := &llmHookTestProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	hook := &llmObserverHook{eventCh: make(chan Event, 1)}
	if err := al.MountHook(NamedHook("llm-observer", hook)); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "hello",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if resp != "hooked content" {
		t.Fatalf("expected hooked content, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "hook-model" {
		t.Fatalf("expected model hook-model, got %q", lastModel)
	}

	select {
	case evt := <-hook.eventCh:
		if evt.Kind != EventKindTurnEnd {
			t.Fatalf("expected turn end event, got %v", evt.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for hook observer event")
	}
}

type clearMessagesHook struct{}

func (h *clearMessagesHook) BeforeLLM(
	ctx context.Context,
	req *LLMHookRequest,
) (*LLMHookRequest, HookDecision, error) {
	next := req.Clone()
	next.Messages = nil
	return next, HookDecision{Action: HookActionModify}, nil
}

func (h *clearMessagesHook) AfterLLM(
	ctx context.Context,
	resp *LLMHookResponse,
) (*LLMHookResponse, HookDecision, error) {
	return resp, HookDecision{Action: HookActionContinue}, nil
}

func TestAgentLoop_Hooks_BeforeLLMCanClearMessagesWithoutPanic(t *testing.T) {
	provider := &llmHookTestProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	if err := al.MountHook(NamedHook("clear-messages", &clearMessagesHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "hello",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if resp != "provider content" {
		t.Fatalf("expected provider content, got %q", resp)
	}

	provider.mu.Lock()
	lastMsgN := provider.lastMsgN
	provider.mu.Unlock()
	if lastMsgN != 0 {
		t.Fatalf("expected provider to receive 0 messages, got %d", lastMsgN)
	}
}

type modelRewriteHook struct {
	model string
}

func (h *modelRewriteHook) BeforeLLM(
	ctx context.Context,
	req *LLMHookRequest,
) (*LLMHookRequest, HookDecision, error) {
	next := req.Clone()
	next.Model = h.model
	return next, HookDecision{Action: HookActionModify}, nil
}

func (h *modelRewriteHook) AfterLLM(
	ctx context.Context,
	resp *LLMHookResponse,
) (*LLMHookResponse, HookDecision, error) {
	return resp, HookDecision{Action: HookActionContinue}, nil
}

func TestAgentLoop_Hooks_BeforeLLMModelRewriteHonoredWithFallbacks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-hooks-fallback-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				Provider:          "openai",
				Model:             "primary-model",
				ModelFallbacks:    []string{"backup-model"},
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	provider := &llmHookTestProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}

	if mountErr := al.MountHook(NamedHook("rewrite-model", &modelRewriteHook{model: "hook-model"})); mountErr != nil {
		t.Fatalf("MountHook failed: %v", mountErr)
	}

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "hello",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if resp != "provider content" {
		t.Fatalf("expected provider content, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "hook-model" {
		t.Fatalf("expected fallback call to use rewritten model, got %q", lastModel)
	}
}

type toolHookProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *toolHookProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{
					ID:        "call-1",
					Name:      "echo_text",
					Arguments: map[string]any{"text": "original"},
				},
			},
		}, nil
	}

	last := messages[len(messages)-1]
	return &providers.LLMResponse{
		Content: last.Content,
	}, nil
}

func (p *toolHookProvider) GetDefaultModel() string {
	return "tool-hook-provider"
}

type echoTextTool struct{}

func (t *echoTextTool) Name() string {
	return "echo_text"
}

func (t *echoTextTool) Description() string {
	return "echo a text argument"
}

func (t *echoTextTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type": "string",
			},
		},
	}
}

func (t *echoTextTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	text, _ := args["text"].(string)
	return tools.SilentResult(text)
}

type toolRewriteHook struct{}

func (h *toolRewriteHook) BeforeTool(
	ctx context.Context,
	call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	next := call.Clone()
	next.Arguments["text"] = "modified"
	return next, HookDecision{Action: HookActionModify}, nil
}

func (h *toolRewriteHook) AfterTool(
	ctx context.Context,
	result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	next := result.Clone()
	next.Result.ForLLM = "after:" + next.Result.ForLLM
	return next, HookDecision{Action: HookActionModify}, nil
}

func TestAgentLoop_Hooks_ToolInterceptorCanRewrite(t *testing.T) {
	provider := &toolHookProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if err := al.MountHook(NamedHook("tool-rewrite", &toolRewriteHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run tool",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if resp != "after:modified" {
		t.Fatalf("expected rewritten tool result, got %q", resp)
	}
}

type beforeToolOnlyRewriteHook struct{}

func (h *beforeToolOnlyRewriteHook) BeforeTool(
	ctx context.Context,
	call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	next := call.Clone()
	next.Arguments["text"] = "modified"
	return next, HookDecision{Action: HookActionModify}, nil
}

func (h *beforeToolOnlyRewriteHook) AfterTool(
	ctx context.Context,
	result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	return result, HookDecision{Action: HookActionContinue}, nil
}

type toolTranscriptProvider struct {
	calls int
}

func (p *toolTranscriptProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{
					ID:        "call-1",
					Name:      "echo_text",
					Arguments: map[string]any{"text": "original"},
				},
			},
		}, nil
	}

	for i := len(messages) - 1; i >= 0; i-- {
		if len(messages[i].ToolCalls) == 0 {
			continue
		}
		return &providers.LLMResponse{
			Content: messages[i].ToolCalls[0].Function.Arguments,
		}, nil
	}

	return &providers.LLMResponse{Content: "missing transcript"}, nil
}

func (p *toolTranscriptProvider) GetDefaultModel() string {
	return "tool-transcript-provider"
}

func TestAgentLoop_Hooks_BeforeToolRewriteUpdatesTranscript(t *testing.T) {
	provider := &toolTranscriptProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if err := al.MountHook(NamedHook("before-tool-only", &beforeToolOnlyRewriteHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run tool",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if resp != `{"text":"modified"}` {
		t.Fatalf("expected rewritten tool call in transcript, got %q", resp)
	}
}

type nestedToolProvider struct {
	calls int
}

func (p *nestedToolProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{
					ID:   "call-1",
					Name: "nested_echo",
					Arguments: map[string]any{
						"payload": map[string]any{"text": "original"},
					},
				},
			},
		}, nil
	}

	return &providers.LLMResponse{
		Content: messages[len(messages)-1].Content,
	}, nil
}

func (p *nestedToolProvider) GetDefaultModel() string {
	return "nested-tool-provider"
}

type nestedEchoTool struct{}

func (t *nestedEchoTool) Name() string {
	return "nested_echo"
}

func (t *nestedEchoTool) Description() string {
	return "echo nested payload text"
}

func (t *nestedEchoTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"payload": map[string]any{
				"type": "object",
			},
		},
	}
}

func (t *nestedEchoTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	payload, _ := args["payload"].(map[string]any)
	text, _ := payload["text"].(string)
	return tools.SilentResult(text)
}

type nestedArgumentMutationHook struct{}

func (h *nestedArgumentMutationHook) BeforeTool(
	ctx context.Context,
	call *ToolCallHookRequest,
) (*ToolCallHookRequest, HookDecision, error) {
	if payload, ok := call.Arguments["payload"].(map[string]any); ok {
		payload["text"] = "mutated"
	}
	return nil, HookDecision{Action: HookActionContinue}, nil
}

func (h *nestedArgumentMutationHook) AfterTool(
	ctx context.Context,
	result *ToolResultHookResponse,
) (*ToolResultHookResponse, HookDecision, error) {
	return result, HookDecision{Action: HookActionContinue}, nil
}

func TestAgentLoop_Hooks_NestedArgumentsAreIsolatedFromHookMutation(t *testing.T) {
	provider := &nestedToolProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	al.RegisterTool(&nestedEchoTool{})
	if err := al.MountHook(NamedHook("nested-arg-mutation", &nestedArgumentMutationHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run nested tool",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if resp != "original" {
		t.Fatalf("expected nested arguments to stay isolated, got %q", resp)
	}
}

type denyApprovalHook struct{}

func (h *denyApprovalHook) ApproveTool(ctx context.Context, req *ToolApprovalRequest) (ApprovalDecision, error) {
	return ApprovalDecision{
		Approved: false,
		Reason:   "blocked",
	}, nil
}

func TestAgentLoop_Hooks_ToolApproverCanDeny(t *testing.T) {
	provider := &toolHookProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if err := al.MountHook(NamedHook("deny-approval", &denyApprovalHook{})); err != nil {
		t.Fatalf("MountHook failed: %v", err)
	}

	sub := al.SubscribeEvents(16)
	defer al.UnsubscribeEvents(sub.ID)

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run tool",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	expected := "Tool execution denied by approval hook: blocked"
	if resp != expected {
		t.Fatalf("expected %q, got %q", expected, resp)
	}

	events := collectEventStream(sub.C)
	skippedEvt, ok := findEvent(events, EventKindToolExecSkipped)
	if !ok {
		t.Fatal("expected tool skipped event")
	}
	payload, ok := skippedEvt.Payload.(ToolExecSkippedPayload)
	if !ok {
		t.Fatalf("expected ToolExecSkippedPayload, got %T", skippedEvt.Payload)
	}
	if payload.Reason != expected {
		t.Fatalf("expected skipped reason %q, got %q", expected, payload.Reason)
	}
}
