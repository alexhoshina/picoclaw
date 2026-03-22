package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pkglogger "github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestProcessHook_HelperProcess(t *testing.T) {
	if os.Getenv("PICOCLAW_HOOK_HELPER") != "1" {
		return
	}
	if err := runProcessHookHelper(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAgentLoop_MountProcessHook_LLMAndObserver(t *testing.T) {
	provider := &llmHookTestProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	eventLog := filepath.Join(t.TempDir(), "events.log")
	if err := al.MountProcessHook(context.Background(), "ipc-llm", ProcessHookOptions{
		Command:      processHookHelperCommand(),
		Env:          processHookHelperEnv("rewrite", eventLog),
		Observe:      true,
		InterceptLLM: true,
	}); err != nil {
		t.Fatalf("MountProcessHook failed: %v", err)
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
	if resp != "provider content|ipc" {
		t.Fatalf("expected process-hooked llm content, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "process-model" {
		t.Fatalf("expected process model, got %q", lastModel)
	}

	waitForFileContains(t, eventLog, "turn_end")
}

func TestAgentLoop_MountProcessHook_ToolRewrite(t *testing.T) {
	provider := &toolHookProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if err := al.MountProcessHook(context.Background(), "ipc-tool", ProcessHookOptions{
		Command:       processHookHelperCommand(),
		Env:           processHookHelperEnv("rewrite", ""),
		InterceptTool: true,
	}); err != nil {
		t.Fatalf("MountProcessHook failed: %v", err)
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
	if resp != "ipc:ipc" {
		t.Fatalf("expected rewritten process-hook tool result, got %q", resp)
	}
}

func TestAgentLoop_MountProcessHook_AfterLLMCanRewriteToolCalls(t *testing.T) {
	provider := &toolHookProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	al.RegisterTool(&echoTextTool{})
	if err := al.MountProcessHook(context.Background(), "ipc-after-llm-tool", ProcessHookOptions{
		Command:      processHookHelperCommand(),
		Env:          processHookHelperEnv("rewrite_after_llm_tool", ""),
		InterceptLLM: true,
	}); err != nil {
		t.Fatalf("MountProcessHook failed: %v", err)
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
	if resp != "ipc-after-llm" {
		t.Fatalf("expected after_llm hook to rewrite tool call, got %q", resp)
	}
}

type blockedToolProvider struct {
	calls int
}

func (p *blockedToolProvider) Chat(
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
					Name:      "blocked_tool",
					Arguments: map[string]any{},
				},
			},
		}, nil
	}

	return &providers.LLMResponse{
		Content: messages[len(messages)-1].Content,
	}, nil
}

func (p *blockedToolProvider) GetDefaultModel() string {
	return "blocked-tool-provider"
}

func TestAgentLoop_MountProcessHook_ApprovalDeny(t *testing.T) {
	provider := &blockedToolProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()

	if err := al.MountProcessHook(context.Background(), "ipc-approval", ProcessHookOptions{
		Command:     processHookHelperCommand(),
		Env:         processHookHelperEnv("deny", ""),
		ApproveTool: true,
	}); err != nil {
		t.Fatalf("MountProcessHook failed: %v", err)
	}

	sub := al.SubscribeEvents(16)
	defer al.UnsubscribeEvents(sub.ID)

	resp, err := al.runAgentLoop(context.Background(), agent, processOptions{
		SessionKey:      "session-1",
		Channel:         "cli",
		ChatID:          "direct",
		UserMessage:     "run blocked tool",
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}

	expected := "Tool execution denied by approval hook: blocked by ipc hook"
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
		t.Fatalf("expected reason %q, got %q", expected, payload.Reason)
	}
}

func TestNewProcessHook_HandshakeTimeoutBounded(t *testing.T) {
	prevTimeout := processHookHelloTimeout
	processHookHelloTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		processHookHelloTimeout = prevTimeout
	})

	start := time.Now()
	_, err := NewProcessHook(context.Background(), "ipc-timeout", ProcessHookOptions{
		Command:      processHookHelperCommand(),
		Env:          processHookHelperEnv("hang_hello", ""),
		InterceptLLM: true,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected handshake deadline exceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected bounded handshake timeout, got %s", elapsed)
	}
}

func TestProcessHook_SendTimeoutClosesTransport(t *testing.T) {
	ph, err := NewProcessHook(context.Background(), "ipc-send-timeout", ProcessHookOptions{
		Command: processHookHelperCommand(),
		Env:     processHookHelperEnv("pause_after_hello", ""),
	})
	if err != nil {
		t.Fatalf("NewProcessHook failed: %v", err)
	}
	defer ph.Close()

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	largePayload := strings.Repeat("x", 8*1024*1024)
	err = ph.send(timeoutCtx, processHookRPCMessage{
		JSONRPC: processHookJSONRPCVersion,
		Method:  "hook.event",
		Params:  json.RawMessage(strconv.Quote(largePayload)),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected send timeout, got %v", err)
	}
	if !ph.closed.Load() {
		t.Fatal("expected timed-out send to close the transport")
	}

	err = ph.send(context.Background(), processHookRPCMessage{
		JSONRPC: processHookJSONRPCVersion,
		Method:  "hook.event",
	})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected subsequent send to fail on closed transport, got %v", err)
	}
}

func TestProcessHook_ReadStderr_TruncatesOutput(t *testing.T) {
	logPath := enableHookProcessTestFileLogger(t)

	ph := &ProcessHook{name: "ipc-stderr-truncate"}
	ph.readStderr(strings.NewReader(strings.Repeat("a", processHookStderrLogLimit) + "\n" + "after-limit-marker\n"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	logs := string(data)
	if !strings.Contains(logs, "Process hook stderr output truncated") {
		t.Fatalf("expected truncation warning in logs, got %q", logs)
	}
	if strings.Contains(logs, "after-limit-marker") {
		t.Fatalf("expected output after truncation to be suppressed, got %q", logs)
	}
}

func TestProcessHook_ReadStderr_LogsReadError(t *testing.T) {
	logPath := enableHookProcessTestFileLogger(t)

	ph := &ProcessHook{name: "ipc-stderr-error"}
	ph.readStderr(failingReader{err: errors.New("stderr boom")})

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	logs := string(data)
	if !strings.Contains(logs, "Process hook stderr stream failed") {
		t.Fatalf("expected stderr stream failure log, got %q", logs)
	}
	if !strings.Contains(logs, "stderr boom") {
		t.Fatalf("expected stderr read error in logs, got %q", logs)
	}
}

func processHookHelperCommand() []string {
	return []string{os.Args[0], "-test.run=TestProcessHook_HelperProcess", "--"}
}

func processHookHelperEnv(mode, eventLog string) []string {
	env := []string{
		"PICOCLAW_HOOK_HELPER=1",
		"PICOCLAW_HOOK_MODE=" + mode,
	}
	if eventLog != "" {
		env = append(env, "PICOCLAW_HOOK_EVENT_LOG="+eventLog)
	}
	return env
}

func waitForFileContains(t *testing.T, path, substring string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), substring) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	data, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %q in %s; current content: %q", substring, path, string(data))
}

func enableHookProcessTestFileLogger(t *testing.T) string {
	t.Helper()

	initialLevel := pkglogger.GetLevel()
	pkglogger.SetLevel(pkglogger.WARN)

	logPath := filepath.Join(t.TempDir(), "hook-process.log")
	if err := pkglogger.EnableFileLogging(logPath); err != nil {
		t.Fatalf("EnableFileLogging failed: %v", err)
	}

	t.Cleanup(func() {
		pkglogger.DisableFileLogging()
		pkglogger.SetLevel(initialLevel)
	})

	return logPath
}

type failingReader struct {
	err error
}

func (r failingReader) Read(_ []byte) (int, error) {
	return 0, r.err
}

func runProcessHookHelper() error {
	mode := os.Getenv("PICOCLAW_HOOK_MODE")
	eventLog := os.Getenv("PICOCLAW_HOOK_EVENT_LOG")
	approveLog := os.Getenv("PICOCLAW_HOOK_APPROVE_LOG")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), processHookReadBufferSize)
	encoder := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		var msg processHookRPCMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return err
		}

		if msg.ID == 0 {
			if msg.Method == "hook.event" && eventLog != "" {
				var evt map[string]any
				if err := json.Unmarshal(msg.Params, &evt); err == nil {
					if kind, ok := evt["Kind"].(string); ok {
						_ = os.WriteFile(eventLog, []byte(kind+"\n"), 0o644)
					}
				}
			}
			continue
		}

		if mode == "hang_hello" && msg.Method == "hook.hello" {
			continue
		}

		result, rpcErr := handleProcessHookRequest(mode, approveLog, msg)
		resp := processHookRPCMessage{
			JSONRPC: processHookJSONRPCVersion,
			ID:      msg.ID,
		}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else if result != nil {
			body, err := json.Marshal(result)
			if err != nil {
				return err
			}
			resp.Result = body
		} else {
			resp.Result = []byte("{}")
		}

		if err := encoder.Encode(resp); err != nil {
			return err
		}

		if mode == "pause_after_hello" && msg.Method == "hook.hello" {
			select {}
		}
	}

	return scanner.Err()
}

func handleProcessHookRequest(mode, approveLog string, msg processHookRPCMessage) (any, *processHookRPCError) {
	switch msg.Method {
	case "hook.hello":
		return map[string]any{"ok": true}, nil
	case "hook.before_llm":
		if mode != "rewrite" {
			return map[string]any{"action": HookActionContinue}, nil
		}
		var req map[string]any
		_ = json.Unmarshal(msg.Params, &req)
		req["model"] = "process-model"
		return map[string]any{
			"action":  HookActionModify,
			"request": req,
		}, nil
	case "hook.after_llm":
		if mode != "rewrite" && mode != "rewrite_after_llm_tool" {
			return map[string]any{"action": HookActionContinue}, nil
		}
		var resp map[string]any
		_ = json.Unmarshal(msg.Params, &resp)
		if rawResponse, ok := resp["response"].(map[string]any); ok {
			if mode == "rewrite" {
				if content, ok := rawResponse["content"].(string); ok {
					rawResponse["content"] = content + "|ipc"
				}
			}
			if mode == "rewrite_after_llm_tool" {
				if rawToolCalls, ok := rawResponse["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
					if rawCall, ok := rawToolCalls[0].(map[string]any); ok {
						if rawCall["name"] == "echo_text" {
							if rawArgs, ok := rawCall["arguments"].(map[string]any); ok {
								rawArgs["text"] = "ipc-after-llm"
								rawCall["arguments"] = rawArgs
							}
						}
					}
				}
			}
		}
		return map[string]any{
			"action":   HookActionModify,
			"response": resp,
		}, nil
	case "hook.before_tool":
		if mode != "rewrite" {
			return map[string]any{"action": HookActionContinue}, nil
		}
		var call map[string]any
		_ = json.Unmarshal(msg.Params, &call)
		rawArgs, ok := call["arguments"].(map[string]any)
		if !ok || rawArgs == nil {
			rawArgs = map[string]any{}
		}
		rawArgs["text"] = "ipc"
		call["arguments"] = rawArgs
		return map[string]any{
			"action": HookActionModify,
			"call":   call,
		}, nil
	case "hook.after_tool":
		if mode != "rewrite" {
			return map[string]any{"action": HookActionContinue}, nil
		}
		var result map[string]any
		_ = json.Unmarshal(msg.Params, &result)
		if rawResult, ok := result["result"].(map[string]any); ok {
			if forLLM, ok := rawResult["for_llm"].(string); ok {
				rawResult["for_llm"] = "ipc:" + forLLM
			}
		}
		return map[string]any{
			"action": HookActionModify,
			"result": result,
		}, nil
	case "hook.approve_tool":
		if mode == "deny" {
			return ApprovalDecision{
				Approved: false,
				Reason:   "blocked by ipc hook",
			}, nil
		}
		if mode == "slow_approve" {
			if approveLog != "" {
				_ = os.WriteFile(approveLog, []byte("approve\n"), 0o644)
			}
			time.Sleep(150 * time.Millisecond)
		}
		return ApprovalDecision{Approved: true}, nil
	default:
		return nil, &processHookRPCError{
			Code:    -32601,
			Message: "method not found",
		}
	}
}
