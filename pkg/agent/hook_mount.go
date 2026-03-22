package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

type hookRuntime struct {
	initOnce sync.Once
	mu       sync.Mutex
	initErr  error
	ready    bool
	mounted  []string
}

func (r *hookRuntime) setInitResult(err error) {
	r.mu.Lock()
	r.initErr = err
	r.ready = err == nil || !isRetryableHookInitErr(err)
	r.mu.Unlock()
}

func (r *hookRuntime) initState() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready, r.initErr
}

func (r *hookRuntime) markInitialized(names []string) {
	r.mu.Lock()
	r.mounted = append([]string(nil), names...)
	r.initErr = nil
	r.ready = true
	r.mu.Unlock()
}

func (r *hookRuntime) snapshotMounted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.mounted...)
}

func (r *hookRuntime) clearRetryableInitFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !isRetryableHookInitErr(r.initErr) {
		return
	}

	r.initErr = nil
	r.ready = false
	r.initOnce = sync.Once{}
}

// BuiltinHookFactory constructs an in-process hook from config.
type BuiltinHookFactory func(ctx context.Context, spec config.BuiltinHookConfig) (any, error)

var (
	builtinHookRegistryMu sync.RWMutex
	builtinHookRegistry   = map[string]BuiltinHookFactory{}
)

// RegisterBuiltinHook registers a named in-process hook factory for config-driven mounting.
func RegisterBuiltinHook(name string, factory BuiltinHookFactory) error {
	if name == "" {
		return fmt.Errorf("builtin hook name is required")
	}
	if factory == nil {
		return fmt.Errorf("builtin hook %q factory is nil", name)
	}

	builtinHookRegistryMu.Lock()
	defer builtinHookRegistryMu.Unlock()

	if _, exists := builtinHookRegistry[name]; exists {
		return fmt.Errorf("builtin hook %q is already registered", name)
	}
	builtinHookRegistry[name] = factory
	return nil
}

func unregisterBuiltinHook(name string) {
	if name == "" {
		return
	}
	builtinHookRegistryMu.Lock()
	delete(builtinHookRegistry, name)
	builtinHookRegistryMu.Unlock()
}

func lookupBuiltinHook(name string) (BuiltinHookFactory, bool) {
	builtinHookRegistryMu.RLock()
	defer builtinHookRegistryMu.RUnlock()

	factory, ok := builtinHookRegistry[name]
	return factory, ok
}

func configureHookManagerFromConfig(hm *HookManager, cfg *config.Config) {
	if hm == nil || cfg == nil {
		return
	}
	hm.ConfigureTimeouts(
		hookTimeoutFromMS(cfg.Hooks.Defaults.ObserverTimeoutMS),
		hookTimeoutFromMS(cfg.Hooks.Defaults.InterceptorTimeoutMS),
		hookTimeoutFromMS(cfg.Hooks.Defaults.ApprovalTimeoutMS),
	)
}

func hookTimeoutFromMS(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

func isRetryableHookInitErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (al *AgentLoop) ensureHooksInitialized(ctx context.Context) error {
	if al == nil || al.cfg == nil || al.hooks == nil {
		return nil
	}

	if ready, err := al.hookRuntime.initState(); ready {
		return err
	}

	al.hookRuntime.initOnce.Do(func() {
		al.hookRuntime.setInitResult(al.loadConfiguredHooks(ctx))
	})

	ready, err := al.hookRuntime.initState()
	if !ready && isRetryableHookInitErr(err) {
		al.hookRuntime.clearRetryableInitFailure()
	}

	return err
}

func (al *AgentLoop) loadConfiguredHooks(ctx context.Context) (err error) {
	regs, err := al.prepareConfiguredHooks(ctx, al.cfg)
	if err != nil {
		return err
	}

	hooksToClose, err := al.applyConfiguredHooks(regs, nil)
	if err != nil {
		for _, reg := range regs {
			closeHookIfPossible(reg.Hook)
		}
		return err
	}
	al.deferCloseConfiguredHooks(hooksToClose)

	return nil
}

func (al *AgentLoop) prepareConfiguredHooks(
	ctx context.Context,
	cfg *config.Config,
) (regs []HookRegistration, err error) {
	if al == nil || cfg == nil || !cfg.Hooks.Enabled {
		return nil, nil
	}

	defer func() {
		if err == nil {
			return
		}
		for _, reg := range regs {
			closeHookIfPossible(reg.Hook)
		}
	}()

	builtinNames := enabledBuiltinHookNames(cfg.Hooks.Builtins)
	processNames := enabledProcessHookNames(cfg.Hooks.Processes)
	if err := validateConfiguredHookNames(builtinNames, processNames); err != nil {
		return nil, err
	}

	for _, name := range builtinNames {
		spec := cfg.Hooks.Builtins[name]
		factory, ok := lookupBuiltinHook(name)
		if !ok {
			return nil, fmt.Errorf("builtin hook %q is not registered", name)
		}

		hook, factoryErr := factory(ctx, spec)
		if factoryErr != nil {
			return nil, fmt.Errorf("build builtin hook %q: %w", name, factoryErr)
		}
		if hook == nil {
			return nil, fmt.Errorf("build builtin hook %q: nil hook", name)
		}
		regs = append(regs, HookRegistration{
			Name:     name,
			Priority: spec.Priority,
			Source:   HookSourceInProcess,
			Hook:     hook,
		})
	}

	for _, name := range processNames {
		spec := cfg.Hooks.Processes[name]
		opts, buildErr := processHookOptionsFromConfig(spec)
		if buildErr != nil {
			return nil, fmt.Errorf("configure process hook %q: %w", name, buildErr)
		}

		processHook, buildErr := NewProcessHook(ctx, name, opts)
		if buildErr != nil {
			return nil, fmt.Errorf("start process hook %q: %w", name, buildErr)
		}
		regs = append(regs, HookRegistration{
			Name:     name,
			Priority: spec.Priority,
			Source:   HookSourceProcess,
			Hook:     processHook,
		})
	}

	return regs, nil
}

func (al *AgentLoop) applyConfiguredHooks(regs []HookRegistration, oldNames []string) ([]any, error) {
	if al == nil {
		return nil, fmt.Errorf("agent loop is nil")
	}
	if al.hooks == nil {
		return nil, fmt.Errorf("hook manager is nil")
	}

	for _, reg := range regs {
		if reg.Name == "" {
			return nil, fmt.Errorf("hook name is required")
		}
		if reg.Hook == nil {
			return nil, fmt.Errorf("hook %q is nil", reg.Name)
		}
	}

	mounted := make([]string, 0, len(regs))
	mountedSet := make(map[string]struct{}, len(regs))
	for _, reg := range regs {
		mounted = append(mounted, reg.Name)
		mountedSet[reg.Name] = struct{}{}
	}

	hooksToClose := make([]any, 0, len(regs)+len(oldNames))
	hm := al.hooks

	hm.mu.Lock()
	nextHooks := make(map[string]HookRegistration, len(hm.hooks)-len(oldNames)+len(regs))
	for name, reg := range hm.hooks {
		nextHooks[name] = reg
	}
	for _, reg := range regs {
		if existing, ok := nextHooks[reg.Name]; ok {
			hooksToClose = append(hooksToClose, existing.Hook)
		}
		nextHooks[reg.Name] = reg
	}
	for _, name := range oldNames {
		if _, ok := mountedSet[name]; ok {
			continue
		}
		if existing, ok := nextHooks[name]; ok {
			hooksToClose = append(hooksToClose, existing.Hook)
			delete(nextHooks, name)
		}
	}
	hm.hooks = nextHooks
	hm.rebuildOrdered()
	hm.mu.Unlock()

	al.hookRuntime.markInitialized(mounted)
	return hooksToClose, nil
}

func (al *AgentLoop) deferCloseConfiguredHooks(hooks []any) {
	if len(hooks) == 0 {
		return
	}

	hooksToClose := append([]any(nil), hooks...)
	if al == nil || al.turnDrains == nil {
		for _, hook := range hooksToClose {
			closeHookIfPossible(hook)
		}
		return
	}

	epoch := al.turnDrains.rotateEpoch()
	go func() {
		al.turnDrains.waitForEpoch(epoch)
		for _, hook := range hooksToClose {
			closeHookIfPossible(hook)
		}
	}()
}

func validateConfiguredHookNames(builtinNames, processNames []string) error {
	if len(builtinNames) == 0 || len(processNames) == 0 {
		return nil
	}

	seen := make(map[string]string, len(builtinNames)+len(processNames))
	for _, name := range builtinNames {
		seen[name] = "builtin"
	}
	for _, name := range processNames {
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("duplicate configured hook name %q across %s and process hooks", name, prev)
		}
		seen[name] = "process"
	}

	return nil
}

func enabledBuiltinHookNames(specs map[string]config.BuiltinHookConfig) []string {
	if len(specs) == 0 {
		return nil
	}

	names := make([]string, 0, len(specs))
	for name, spec := range specs {
		if spec.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func enabledProcessHookNames(specs map[string]config.ProcessHookConfig) []string {
	if len(specs) == 0 {
		return nil
	}

	names := make([]string, 0, len(specs))
	for name, spec := range specs {
		if spec.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func processHookOptionsFromConfig(spec config.ProcessHookConfig) (ProcessHookOptions, error) {
	transport := spec.Transport
	if transport == "" {
		transport = "stdio"
	}
	if transport != "stdio" {
		return ProcessHookOptions{}, fmt.Errorf("unsupported transport %q", transport)
	}
	if len(spec.Command) == 0 {
		return ProcessHookOptions{}, fmt.Errorf("command is required")
	}

	opts := ProcessHookOptions{
		Command: append([]string(nil), spec.Command...),
		Dir:     spec.Dir,
		Env:     processHookEnvFromMap(spec.Env),
	}

	observeKinds, observeEnabled, err := processHookObserveKindsFromConfig(spec.Observe)
	if err != nil {
		return ProcessHookOptions{}, err
	}
	opts.Observe = observeEnabled
	opts.ObserveKinds = observeKinds

	for _, intercept := range spec.Intercept {
		switch intercept {
		case "before_llm", "after_llm":
			opts.InterceptLLM = true
		case "before_tool", "after_tool":
			opts.InterceptTool = true
		case "approve_tool":
			opts.ApproveTool = true
		case "":
			continue
		default:
			return ProcessHookOptions{}, fmt.Errorf("unsupported intercept %q", intercept)
		}
	}

	if !opts.Observe && !opts.InterceptLLM && !opts.InterceptTool && !opts.ApproveTool {
		return ProcessHookOptions{}, fmt.Errorf("no hook modes enabled")
	}

	return opts, nil
}

func processHookEnvFromMap(envMap map[string]string) []string {
	if len(envMap) == 0 {
		return nil
	}

	keys := make([]string, 0, len(envMap))
	for key := range envMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+envMap[key])
	}
	return env
}

func processHookObserveKindsFromConfig(observe []string) ([]string, bool, error) {
	if len(observe) == 0 {
		return nil, false, nil
	}

	validKinds := validHookEventKinds()
	normalized := make([]string, 0, len(observe))
	for _, kind := range observe {
		switch kind {
		case "", "*", "all":
			return nil, true, nil
		default:
			if _, ok := validKinds[kind]; !ok {
				return nil, false, fmt.Errorf("unsupported observe event %q", kind)
			}
			normalized = append(normalized, kind)
		}
	}

	if len(normalized) == 0 {
		return nil, false, nil
	}
	return normalized, true, nil
}

func validHookEventKinds() map[string]struct{} {
	kinds := make(map[string]struct{}, int(eventKindCount))
	for kind := EventKind(0); kind < eventKindCount; kind++ {
		kinds[kind.String()] = struct{}{}
	}
	return kinds
}
