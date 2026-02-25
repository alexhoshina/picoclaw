// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package channels

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// retryAction determines how to handle a send error.
type retryAction int

const (
	retryActionStop               retryAction = iota // permanent error, stop retrying
	retryActionFixedDelay                            // rate limit, use fixed delay
	retryActionExponentialBackoff                    // temporary error, use exponential backoff
)

// classifyError determines the retry action for a given error.
func classifyError(err error) retryAction {
	if errors.Is(err, ErrNotRunning) || errors.Is(err, ErrSendFailed) {
		return retryActionStop
	}
	if errors.Is(err, ErrRateLimit) {
		return retryActionFixedDelay
	}
	return retryActionExponentialBackoff
}

// retrySend executes sendFunc with retry logic based on error classification.
func retrySend(ctx context.Context, sendFunc func(context.Context) error, name, chatID string) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		lastErr = sendFunc(ctx)
		if lastErr == nil {
			return nil
		}

		action := classifyError(lastErr)

		// Permanent failures — don't retry
		if action == retryActionStop {
			break
		}

		// Last attempt exhausted — don't sleep
		if attempt == maxRetries {
			break
		}

		// Apply backoff based on error type
		var backoff time.Duration
		if action == retryActionFixedDelay {
			backoff = rateLimitDelay
		} else {
			backoff = min(time.Duration(float64(baseBackoff)*math.Pow(2, float64(attempt))), maxBackoff)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// All retries exhausted or permanent failure
	logger.ErrorCF("channels", "Send failed after retries", map[string]any{
		"channel": name,
		"chat_id": chatID,
		"error":   lastErr.Error(),
		"retries": maxRetries,
	})
	return lastErr
}

// sendWithRetry sends a message with rate limiting, preSend, and retry logic.
func (m *Manager) sendWithRetry(ctx context.Context, name string, w *channelWorker, msg bus.OutboundMessage) {
	// Rate limit: wait for token
	if err := w.limiter.Wait(ctx); err != nil {
		return // ctx cancelled
	}

	// Pre-send: stop typing and try to edit placeholder
	if m.preSend(ctx, name, msg, w.ch) {
		return // placeholder edited, skip Send
	}

	// Send with retry
	retrySend(ctx, func(ctx context.Context) error {
		return w.ch.Send(ctx, msg)
	}, name, msg.ChatID)
}

// sendMediaWithRetry sends media with rate limiting and retry logic.
func (m *Manager) sendMediaWithRetry(ctx context.Context, name string, w *channelWorker, msg bus.OutboundMediaMessage) {
	ms, ok := w.ch.(MediaSender)
	if !ok {
		logger.DebugCF("channels", "Channel does not support MediaSender", map[string]any{
			"channel": name,
		})
		return
	}

	// Rate limit: wait for token
	if err := w.limiter.Wait(ctx); err != nil {
		return // ctx cancelled
	}

	// Send with retry
	retrySend(ctx, func(ctx context.Context) error {
		return ms.SendMedia(ctx, msg)
	}, name, msg.ChatID)
}
