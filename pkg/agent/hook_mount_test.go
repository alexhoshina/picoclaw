package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type builtinAutoHookConfig struct {
	Model  string `json:"model"`
	Suffix string `json:"suffix"`
}

type builtinAutoHook struct {
	model  string
	suffix string
}

func (h *builtinAutoHook) BeforeLLM(
	ctx context.Context,
	req *LLMHookRequest,
) (*LLMHookRequest, HookDecision, error) {
	next := req.Clone()
	next.Model = h.model
	return next, HookDecision{Action: HookActionModify}, nil
}

func (h *builtinAutoHook) AfterLLM(
	ctx context.Context,
	resp *LLMHookResponse,
) (*LLMHookResponse, HookDecision, error) {
	next := resp.Clone()
	if next.Response != nil {
		next.Response.Content += h.suffix
	}
	return next, HookDecision{Action: HookActionModify}, nil
}

func newConfiguredHookLoop(t *testing.T, provider providers.LLMProvider, hooks config.HooksConfig) *AgentLoop {
	t.Helper()

	cfg := newConfiguredHookConfig(t, hooks)

	return NewAgentLoop(cfg, bus.NewMessageBus(), provider)
}

func newConfiguredHookConfig(t *testing.T, hooks config.HooksConfig) *config.Config {
	t.Helper()

	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				Model:             "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
		Hooks: hooks,
	}
}

func TestAgentLoop_ProcessDirectWithChannel_AutoMountsBuiltinHook(t *testing.T) {
	const hookName = "test-auto-builtin-hook"

	if err := RegisterBuiltinHook(hookName, func(
		ctx context.Context,
		spec config.BuiltinHookConfig,
	) (any, error) {
		var hookCfg builtinAutoHookConfig
		if len(spec.Config) > 0 {
			if err := json.Unmarshal(spec.Config, &hookCfg); err != nil {
				return nil, err
			}
		}
		return &builtinAutoHook{
			model:  hookCfg.Model,
			suffix: hookCfg.Suffix,
		}, nil
	}); err != nil {
		t.Fatalf("RegisterBuiltinHook failed: %v", err)
	}
	t.Cleanup(func() {
		unregisterBuiltinHook(hookName)
	})

	rawCfg, err := json.Marshal(builtinAutoHookConfig{
		Model:  "builtin-model",
		Suffix: "|builtin",
	})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	provider := &llmHookTestProvider{}
	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			hookName: {
				Enabled: true,
				Config:  rawCfg,
			},
		},
	})
	defer al.Close()

	resp, err := al.ProcessDirectWithChannel(context.Background(), "hello", "session-1", "cli", "direct")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel failed: %v", err)
	}
	if resp != "provider content|builtin" {
		t.Fatalf("expected builtin-hooked content, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "builtin-model" {
		t.Fatalf("expected builtin model, got %q", lastModel)
	}
}

func TestAgentLoop_ProcessDirectWithChannel_AutoMountsProcessHook(t *testing.T) {
	provider := &llmHookTestProvider{}
	eventLog := filepath.Join(t.TempDir(), "events.log")

	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Processes: map[string]config.ProcessHookConfig{
			"ipc-auto": {
				Enabled: true,
				Command: processHookHelperCommand(),
				Env: map[string]string{
					"PICOCLAW_HOOK_HELPER":    "1",
					"PICOCLAW_HOOK_MODE":      "rewrite",
					"PICOCLAW_HOOK_EVENT_LOG": eventLog,
				},
				Observe:   []string{"turn_end"},
				Intercept: []string{"before_llm", "after_llm"},
			},
		},
	})
	defer al.Close()

	resp, err := al.ProcessDirectWithChannel(context.Background(), "hello", "session-1", "cli", "direct")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel failed: %v", err)
	}
	if resp != "provider content|ipc" {
		t.Fatalf("expected process-hooked content, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "process-model" {
		t.Fatalf("expected process model, got %q", lastModel)
	}

	waitForFileContains(t, eventLog, "turn_end")
}

func TestAgentLoop_ProcessDirectWithChannel_InvalidConfiguredHookFails(t *testing.T) {
	provider := &llmHookTestProvider{}
	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Processes: map[string]config.ProcessHookConfig{
			"bad-hook": {
				Enabled:   true,
				Command:   processHookHelperCommand(),
				Intercept: []string{"not_supported"},
			},
		},
	})
	defer al.Close()

	_, err := al.ProcessDirectWithChannel(context.Background(), "hello", "session-1", "cli", "direct")
	if err == nil {
		t.Fatal("expected invalid configured hook error")
	}
}

func TestAgentLoop_ProcessDirectWithChannel_DuplicateConfiguredHookNamesFail(t *testing.T) {
	const hookName = "duplicate-hook"

	if err := RegisterBuiltinHook(hookName, func(
		ctx context.Context,
		spec config.BuiltinHookConfig,
	) (any, error) {
		return &builtinAutoHook{
			model:  "builtin-model",
			suffix: "|builtin",
		}, nil
	}); err != nil {
		t.Fatalf("RegisterBuiltinHook failed: %v", err)
	}
	t.Cleanup(func() {
		unregisterBuiltinHook(hookName)
	})

	provider := &llmHookTestProvider{}
	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			hookName: {Enabled: true},
		},
		Processes: map[string]config.ProcessHookConfig{
			hookName: {
				Enabled:   true,
				Command:   processHookHelperCommand(),
				Intercept: []string{"before_llm"},
			},
		},
	})
	defer al.Close()

	_, err := al.ProcessDirectWithChannel(context.Background(), "hello", "session-1", "cli", "direct")
	if err == nil {
		t.Fatal("expected duplicate configured hook name to fail")
	}
}

func TestAgentLoop_EnsureHooksInitialized_RetriesAfterContextTimeout(t *testing.T) {
	const hookName = "test-init-retry-builtin-hook"

	var attempts int
	if err := RegisterBuiltinHook(hookName, func(
		ctx context.Context,
		spec config.BuiltinHookConfig,
	) (any, error) {
		attempts++
		if attempts == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &builtinAutoHook{
			model:  "retry-model",
			suffix: "|retry",
		}, nil
	}); err != nil {
		t.Fatalf("RegisterBuiltinHook failed: %v", err)
	}
	t.Cleanup(func() {
		unregisterBuiltinHook(hookName)
	})

	provider := &llmHookTestProvider{}
	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			hookName: {Enabled: true},
		},
	})
	defer al.Close()

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := al.ensureHooksInitialized(timeoutCtx); err == nil {
		t.Fatal("expected first initialization attempt to time out")
	}

	if err := al.ensureHooksInitialized(context.Background()); err != nil {
		t.Fatalf("expected second initialization attempt to succeed, got %v", err)
	}

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
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
	if resp != "provider content|retry" {
		t.Fatalf("expected retried hook content, got %q", resp)
	}
}

func TestAgentLoop_ReloadProviderAndConfig_RemountsConfiguredHooks(t *testing.T) {
	const hookName = "test-auto-reload-builtin-hook"

	if err := RegisterBuiltinHook(hookName, func(
		ctx context.Context,
		spec config.BuiltinHookConfig,
	) (any, error) {
		var hookCfg builtinAutoHookConfig
		if len(spec.Config) > 0 {
			if err := json.Unmarshal(spec.Config, &hookCfg); err != nil {
				return nil, err
			}
		}
		return &builtinAutoHook{
			model:  hookCfg.Model,
			suffix: hookCfg.Suffix,
		}, nil
	}); err != nil {
		t.Fatalf("RegisterBuiltinHook failed: %v", err)
	}
	t.Cleanup(func() {
		unregisterBuiltinHook(hookName)
	})

	initialRawCfg, err := json.Marshal(builtinAutoHookConfig{
		Model:  "initial-model",
		Suffix: "|initial",
	})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	reloadedRawCfg, err := json.Marshal(builtinAutoHookConfig{
		Model:  "reloaded-model",
		Suffix: "|reloaded",
	})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	provider := &llmHookTestProvider{}
	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			hookName: {
				Enabled: true,
				Config:  initialRawCfg,
			},
		},
	})
	defer al.Close()

	if initErr := al.ensureHooksInitialized(context.Background()); initErr != nil {
		t.Fatalf("ensureHooksInitialized failed: %v", initErr)
	}

	reloadedCfg := newConfiguredHookConfig(t, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			hookName: {
				Enabled: true,
				Config:  reloadedRawCfg,
			},
		},
	})

	if reloadErr := al.ReloadProviderAndConfig(context.Background(), provider, reloadedCfg); reloadErr != nil {
		t.Fatalf("ReloadProviderAndConfig failed: %v", reloadErr)
	}

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
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
	if resp != "provider content|reloaded" {
		t.Fatalf("expected reloaded hook content, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "reloaded-model" {
		t.Fatalf("expected reloaded hook model, got %q", lastModel)
	}
}

func TestAgentLoop_ReloadProviderAndConfig_FailedHookReloadLeavesOldHooksActive(t *testing.T) {
	const hookName = "test-auto-reload-rollback-builtin-hook"
	const brokenHookName = "test-auto-reload-broken-builtin-hook"

	if err := RegisterBuiltinHook(hookName, func(
		ctx context.Context,
		spec config.BuiltinHookConfig,
	) (any, error) {
		var hookCfg builtinAutoHookConfig
		if len(spec.Config) > 0 {
			if err := json.Unmarshal(spec.Config, &hookCfg); err != nil {
				return nil, err
			}
		}
		return &builtinAutoHook{
			model:  hookCfg.Model,
			suffix: hookCfg.Suffix,
		}, nil
	}); err != nil {
		t.Fatalf("RegisterBuiltinHook failed: %v", err)
	}
	t.Cleanup(func() {
		unregisterBuiltinHook(hookName)
	})

	if err := RegisterBuiltinHook(brokenHookName, func(
		ctx context.Context,
		spec config.BuiltinHookConfig,
	) (any, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("RegisterBuiltinHook failed: %v", err)
	}
	t.Cleanup(func() {
		unregisterBuiltinHook(brokenHookName)
	})

	initialRawCfg, err := json.Marshal(builtinAutoHookConfig{
		Model:  "stable-model",
		Suffix: "|stable",
	})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	provider := &llmHookTestProvider{}
	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			hookName: {
				Enabled: true,
				Config:  initialRawCfg,
			},
		},
	})
	defer al.Close()

	if initErr := al.ensureHooksInitialized(context.Background()); initErr != nil {
		t.Fatalf("ensureHooksInitialized failed: %v", initErr)
	}

	reloadedCfg := newConfiguredHookConfig(t, config.HooksConfig{
		Enabled: true,
		Builtins: map[string]config.BuiltinHookConfig{
			brokenHookName: {Enabled: true},
		},
	})
	reloadedCfg.Agents.Defaults.Model = "broken-model"

	if reloadErr := al.ReloadProviderAndConfig(context.Background(), provider, reloadedCfg); reloadErr == nil {
		t.Fatal("expected reload with invalid hook to fail")
	}

	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
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
	if resp != "provider content|stable" {
		t.Fatalf("expected old hook to remain active after failed reload, got %q", resp)
	}

	provider.mu.Lock()
	lastModel := provider.lastModel
	provider.mu.Unlock()
	if lastModel != "stable-model" {
		t.Fatalf("expected old hook model to remain active, got %q", lastModel)
	}
}

func TestAgentLoop_ReloadProviderAndConfig_KeepsOldConfiguredHooksAliveForActiveTurn(t *testing.T) {
	provider := &toolHookProvider{}
	approveLog := filepath.Join(t.TempDir(), "approve.log")

	al := newConfiguredHookLoop(t, provider, config.HooksConfig{
		Enabled: true,
		Processes: map[string]config.ProcessHookConfig{
			"ipc-approval": {
				Enabled: true,
				Command: processHookHelperCommand(),
				Env: map[string]string{
					"PICOCLAW_HOOK_HELPER":      "1",
					"PICOCLAW_HOOK_MODE":        "slow_approve",
					"PICOCLAW_HOOK_APPROVE_LOG": approveLog,
				},
				Intercept: []string{"approve_tool"},
			},
		},
	})
	al.RegisterTool(&echoTextTool{})
	defer al.Close()

	type runResult struct {
		resp string
		err  error
	}
	resultCh := make(chan runResult, 1)
	go func() {
		resp, err := al.ProcessDirectWithChannel(context.Background(), "run tool", "session-1", "cli", "direct")
		resultCh <- runResult{resp: resp, err: err}
	}()

	waitForFileContains(t, approveLog, "approve")

	reloadedCfg := newConfiguredHookConfig(t, config.HooksConfig{
		Enabled: true,
	})

	reloadCh := make(chan error, 1)
	go func() {
		reloadCh <- al.ReloadProviderAndConfig(context.Background(), &toolHookProvider{}, reloadedCfg)
	}()

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("ProcessDirectWithChannel failed: %v", result.err)
	}
	if result.resp != "original" {
		t.Fatalf("expected active turn to complete with old hook approval, got %q", result.resp)
	}

	select {
	case err := <-reloadCh:
		if err != nil {
			t.Fatalf("ReloadProviderAndConfig failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reload to finish")
	}
}
