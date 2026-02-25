// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package channels

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/constants"
	"github.com/sipeed/picoclaw/pkg/health"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
)

const (
	defaultChannelQueueSize = 16
	defaultRateLimit        = 10 // default 10 msg/s
	maxRetries              = 3
	rateLimitDelay          = 1 * time.Second
	baseBackoff             = 500 * time.Millisecond
	maxBackoff              = 8 * time.Second

	janitorInterval = 10 * time.Second
	typingStopTTL   = 5 * time.Minute
	placeholderTTL  = 10 * time.Minute
)

// channelRateConfig maps channel name to per-second rate limit.
var channelRateConfig = map[string]float64{
	"telegram": 20,
	"discord":  1,
	"slack":    1,
	"line":     10,
}

type Manager struct {
	channels     map[string]Channel
	workers      map[string]*channelWorker
	bus          *bus.MessageBus
	config       *config.Config
	mediaStore   media.MediaStore
	dispatchTask *asyncTask
	mux          *http.ServeMux
	httpServer   *http.Server
	mu           sync.RWMutex
	placeholders sync.Map // "channel:chatID" → placeholderID (string)
	typingStops  sync.Map // "channel:chatID" → func()
}

type asyncTask struct {
	cancel context.CancelFunc
}

// channelSpec defines a channel initialization specification for table-driven init.
type channelSpec struct {
	key         string // internal channel name (e.g., "telegram")
	displayName string // user-facing name (e.g., "Telegram")
	ready       func(*config.Config) bool
}

// RecordPlaceholder registers a placeholder message for later editing.
// Implements PlaceholderRecorder.
func (m *Manager) RecordPlaceholder(channel, chatID, placeholderID string) {
	key := channel + ":" + chatID
	m.placeholders.Store(key, placeholderEntry{id: placeholderID, createdAt: time.Now()})
}

// RecordTypingStop registers a typing stop function for later invocation.
// Implements PlaceholderRecorder.
func (m *Manager) RecordTypingStop(channel, chatID string, stop func()) {
	key := channel + ":" + chatID
	m.typingStops.Store(key, typingEntry{stop: stop, createdAt: time.Now()})
}

func NewManager(cfg *config.Config, messageBus *bus.MessageBus, store media.MediaStore) (*Manager, error) {
	m := &Manager{
		channels:   make(map[string]Channel),
		workers:    make(map[string]*channelWorker),
		bus:        messageBus,
		config:     cfg,
		mediaStore: store,
	}

	if err := m.initChannels(); err != nil {
		return nil, err
	}

	return m, nil
}

// initChannel is a helper that looks up a factory by name and creates the channel.
func (m *Manager) initChannel(name, displayName string) {
	f, ok := getFactory(name)
	if !ok {
		logger.WarnCF("channels", "Factory not registered", map[string]any{
			"channel": displayName,
		})
		return
	}
	logger.DebugCF("channels", "Attempting to initialize channel", map[string]any{
		"channel": displayName,
	})
	ch, err := f(m.config, m.bus)
	if err != nil {
		logger.ErrorCF("channels", "Failed to initialize channel", map[string]any{
			"channel": displayName,
			"error":   err.Error(),
		})
	} else {
		// Inject MediaStore if channel supports it
		if m.mediaStore != nil {
			if setter, ok := ch.(interface{ SetMediaStore(s media.MediaStore) }); ok {
				setter.SetMediaStore(m.mediaStore)
			}
		}
		// Inject PlaceholderRecorder if channel supports it
		if setter, ok := ch.(interface{ SetPlaceholderRecorder(r PlaceholderRecorder) }); ok {
			setter.SetPlaceholderRecorder(m)
		}
		m.channels[name] = ch
		logger.InfoCF("channels", "Channel enabled successfully", map[string]any{
			"channel": displayName,
		})
	}
}

func (m *Manager) initChannels() error {
	logger.InfoC("channels", "Initializing channel manager")

	// Table-driven channel initialization (reduces cyclomatic complexity from 24→3)
	specs := []channelSpec{
		{
			key:         "telegram",
			displayName: "Telegram",
			ready:       func(c *config.Config) bool { return c.Channels.Telegram.Enabled && c.Channels.Telegram.Token != "" },
		},
		{
			key:         "whatsapp",
			displayName: "WhatsApp",
			ready:       func(c *config.Config) bool { return c.Channels.WhatsApp.Enabled && c.Channels.WhatsApp.BridgeURL != "" },
		},
		{
			key:         "feishu",
			displayName: "Feishu",
			ready:       func(c *config.Config) bool { return c.Channels.Feishu.Enabled },
		},
		{
			key:         "discord",
			displayName: "Discord",
			ready:       func(c *config.Config) bool { return c.Channels.Discord.Enabled && c.Channels.Discord.Token != "" },
		},
		{
			key:         "maixcam",
			displayName: "MaixCam",
			ready:       func(c *config.Config) bool { return c.Channels.MaixCam.Enabled },
		},
		{
			key:         "qq",
			displayName: "QQ",
			ready:       func(c *config.Config) bool { return c.Channels.QQ.Enabled },
		},
		{
			key:         "dingtalk",
			displayName: "DingTalk",
			ready:       func(c *config.Config) bool { return c.Channels.DingTalk.Enabled && c.Channels.DingTalk.ClientID != "" },
		},
		{
			key:         "slack",
			displayName: "Slack",
			ready:       func(c *config.Config) bool { return c.Channels.Slack.Enabled && c.Channels.Slack.BotToken != "" },
		},
		{
			key:         "line",
			displayName: "LINE",
			ready: func(c *config.Config) bool {
				return c.Channels.LINE.Enabled && c.Channels.LINE.ChannelAccessToken != ""
			},
		},
		{
			key:         "onebot",
			displayName: "OneBot",
			ready:       func(c *config.Config) bool { return c.Channels.OneBot.Enabled && c.Channels.OneBot.WSUrl != "" },
		},
		{
			key:         "wecom",
			displayName: "WeCom",
			ready:       func(c *config.Config) bool { return c.Channels.WeCom.Enabled && c.Channels.WeCom.Token != "" },
		},
		{
			key:         "wecom_app",
			displayName: "WeCom App",
			ready:       func(c *config.Config) bool { return c.Channels.WeComApp.Enabled && c.Channels.WeComApp.CorpID != "" },
		},
		{
			key:         "pico",
			displayName: "Pico",
			ready:       func(c *config.Config) bool { return c.Channels.Pico.Enabled && c.Channels.Pico.Token != "" },
		},
	}

	for _, s := range specs {
		if s.ready(m.config) {
			m.initChannel(s.key, s.displayName)
		}
	}

	logger.InfoCF("channels", "Channel initialization completed", map[string]any{
		"enabled_channels": len(m.channels),
	})

	return nil
}

// SetupHTTPServer creates a shared HTTP server with the given listen address.
// It registers health endpoints from the health server and discovers channels
// that implement WebhookHandler and/or HealthChecker to register their handlers.
func (m *Manager) SetupHTTPServer(addr string, healthServer *health.Server) {
	m.mux = http.NewServeMux()

	// Register health endpoints
	if healthServer != nil {
		healthServer.RegisterOnMux(m.mux)
	}

	// Discover and register webhook handlers and health checkers
	for name, ch := range m.channels {
		if wh, ok := ch.(WebhookHandler); ok {
			m.mux.Handle(wh.WebhookPath(), wh)
			logger.InfoCF("channels", "Webhook handler registered", map[string]any{
				"channel": name,
				"path":    wh.WebhookPath(),
			})
		}
		if hc, ok := ch.(HealthChecker); ok {
			m.mux.HandleFunc(hc.HealthPath(), hc.HealthHandler)
			logger.InfoCF("channels", "Health endpoint registered", map[string]any{
				"channel": name,
				"path":    hc.HealthPath(),
			})
		}
	}

	m.httpServer = &http.Server{
		Addr:         addr,
		Handler:      m.mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
}

// dispatchGeneric is a generic dispatcher that handles both OutboundMessage and OutboundMediaMessage.
// It eliminates code duplication between dispatchOutbound and dispatchOutboundMedia.
func dispatchGeneric[T any](
	m *Manager,
	ctx context.Context,
	subscribe func(context.Context) (T, bool),
	getChannel func(T) string,
	getQueue func(*channelWorker) chan T,
	logName string,
) {
	logger.InfoCF("channels", logName+" started", nil)

	for {
		msg, ok := subscribe(ctx)
		if !ok {
			logger.InfoCF("channels", logName+" stopped", nil)
			return
		}

		channel := getChannel(msg)

		// Silently skip internal channels
		if constants.IsInternalChannel(channel) {
			continue
		}

		m.mu.RLock()
		_, exists := m.channels[channel]
		w, wExists := m.workers[channel]
		m.mu.RUnlock()

		if !exists {
			logger.WarnCF("channels", "Unknown channel for "+logName, map[string]any{
				"channel": channel,
			})
			continue
		}

		if wExists && w != nil {
			queue := getQueue(w)
			select {
			case queue <- msg:
			case <-ctx.Done():
				return
			}
		} else if exists {
			logger.WarnCF("channels", "Channel has no active worker, skipping message", map[string]any{
				"channel": channel,
			})
		}
	}
}

func (m *Manager) dispatchOutbound(ctx context.Context) {
	dispatchGeneric(
		m,
		ctx,
		m.bus.SubscribeOutbound,
		func(msg bus.OutboundMessage) string { return msg.Channel },
		func(w *channelWorker) chan bus.OutboundMessage { return w.queue },
		"Outbound dispatcher",
	)
}

func (m *Manager) dispatchOutboundMedia(ctx context.Context) {
	dispatchGeneric(
		m,
		ctx,
		m.bus.SubscribeOutboundMedia,
		func(msg bus.OutboundMediaMessage) string { return msg.Channel },
		func(w *channelWorker) chan bus.OutboundMediaMessage { return w.mediaQueue },
		"Outbound media dispatcher",
	)
}

func (m *Manager) GetChannel(name string) (Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	channel, ok := m.channels[name]
	return channel, ok
}

func (m *Manager) GetStatus() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := make(map[string]any)
	for name, channel := range m.channels {
		status[name] = map[string]any{
			"enabled": true,
			"running": channel.IsRunning(),
		}
	}
	return status
}

func (m *Manager) GetEnabledChannels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.channels))
	for name := range m.channels {
		names = append(names, name)
	}
	return names
}

func (m *Manager) RegisterChannel(name string, channel Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels[name] = channel
}

func (m *Manager) UnregisterChannel(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.workers[name]; ok && w != nil {
		close(w.queue)
		<-w.done
		close(w.mediaQueue)
		<-w.mediaDone
	}
	delete(m.workers, name)
	delete(m.channels, name)
}

func (m *Manager) SendToChannel(ctx context.Context, channelName, chatID, content string) error {
	m.mu.RLock()
	_, exists := m.channels[channelName]
	w, wExists := m.workers[channelName]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("channel %s not found", channelName)
	}

	msg := bus.OutboundMessage{
		Channel: channelName,
		ChatID:  chatID,
		Content: content,
	}

	if wExists && w != nil {
		select {
		case w.queue <- msg:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Fallback: direct send (should not happen)
	channel, _ := m.channels[channelName]
	return channel.Send(ctx, msg)
}
