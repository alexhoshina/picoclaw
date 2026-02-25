// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package channels

import (
	"context"
	"net/http"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// startChannelWithWorkers starts a channel and its associated workers.
func (m *Manager) startChannelWithWorkers(
	ctx context.Context,
	dispatchCtx context.Context,
	name string,
	channel Channel,
) error {
	if err := channel.Start(ctx); err != nil {
		return err
	}
	w := newChannelWorker(name, channel)
	m.workers[name] = w
	go m.runWorker(dispatchCtx, name, w)
	go m.runMediaWorker(dispatchCtx, name, w)
	return nil
}

// startDispatchers starts the outbound message dispatchers and janitor.
func (m *Manager) startDispatchers(ctx context.Context) {
	go m.dispatchOutbound(ctx)
	go m.dispatchOutboundMedia(ctx)
	go m.runTTLJanitor(ctx)
}

// startHTTPServerAsync starts the shared HTTP server in a goroutine.
func (m *Manager) startHTTPServerAsync() {
	if m.httpServer == nil {
		return
	}
	go func() {
		logger.InfoCF("channels", "Shared HTTP server listening", map[string]any{
			"addr": m.httpServer.Addr,
		})
		if err := m.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.ErrorCF("channels", "Shared HTTP server error", map[string]any{
				"error": err.Error(),
			})
		}
	}()
}

// StartAll starts all enabled channels and supporting services.
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.channels) == 0 {
		logger.WarnC("channels", "No channels enabled")
		return nil
	}

	logger.InfoC("channels", "Starting all channels")

	dispatchCtx, cancel := context.WithCancel(ctx)
	m.dispatchTask = &asyncTask{cancel: cancel}

	// Start each channel with workers
	for name, channel := range m.channels {
		logger.InfoCF("channels", "Starting channel", map[string]any{"channel": name})
		if err := m.startChannelWithWorkers(ctx, dispatchCtx, name, channel); err != nil {
			logger.ErrorCF("channels", "Failed to start channel", map[string]any{
				"channel": name,
				"error":   err.Error(),
			})
			continue
		}
	}

	// Start dispatchers and janitor
	m.startDispatchers(dispatchCtx)

	// Start HTTP server
	m.startHTTPServerAsync()

	logger.InfoC("channels", "All channels started")
	return nil
}

// shutdownHTTPServer gracefully shuts down the HTTP server.
func (m *Manager) shutdownHTTPServer(ctx context.Context) {
	if m.httpServer == nil {
		return
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := m.httpServer.Shutdown(shutdownCtx); err != nil {
		logger.ErrorCF("channels", "Shared HTTP server shutdown error", map[string]any{
			"error": err.Error(),
		})
	}
	m.httpServer = nil
}

// cancelDispatchers stops the dispatch task.
func (m *Manager) cancelDispatchers() {
	if m.dispatchTask != nil {
		m.dispatchTask.cancel()
		m.dispatchTask = nil
	}
}

// drainWorkers closes all worker queues and waits for them to exit.
func (m *Manager) drainWorkers() {
	// Close text queues
	for _, w := range m.workers {
		if w != nil {
			close(w.queue)
		}
	}
	// Wait for text workers to exit
	for _, w := range m.workers {
		if w != nil {
			<-w.done
		}
	}
	// Close media queues
	for _, w := range m.workers {
		if w != nil {
			close(w.mediaQueue)
		}
	}
	// Wait for media workers to exit
	for _, w := range m.workers {
		if w != nil {
			<-w.mediaDone
		}
	}
}

// stopChannels calls Stop on all channels.
func (m *Manager) stopChannels(ctx context.Context) {
	for name, channel := range m.channels {
		logger.InfoCF("channels", "Stopping channel", map[string]any{"channel": name})
		if err := channel.Stop(ctx); err != nil {
			logger.ErrorCF("channels", "Error stopping channel", map[string]any{
				"channel": name,
				"error":   err.Error(),
			})
		}
	}
}

// StopAll stops all channels and supporting services.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	logger.InfoC("channels", "Stopping all channels")

	m.shutdownHTTPServer(ctx)
	m.cancelDispatchers()
	m.drainWorkers()
	m.stopChannels(ctx)

	logger.InfoC("channels", "All channels stopped")
	return nil
}
