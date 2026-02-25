// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package channels

import (
	"context"
	"time"
)

// evictTypingEntries removes typing stop entries that have exceeded TTL.
func (m *Manager) evictTypingEntries(now time.Time) {
	m.typingStops.Range(func(key, value any) bool {
		entry, ok := value.(typingEntry)
		if !ok {
			return true
		}
		if now.Sub(entry.createdAt) <= typingStopTTL {
			return true // not expired yet
		}
		if _, loaded := m.typingStops.LoadAndDelete(key); loaded {
			entry.stop() // idempotent, safe
		}
		return true
	})
}

// evictPlaceholderEntries removes placeholder entries that have exceeded TTL.
func (m *Manager) evictPlaceholderEntries(now time.Time) {
	m.placeholders.Range(func(key, value any) bool {
		entry, ok := value.(placeholderEntry)
		if !ok {
			return true
		}
		if now.Sub(entry.createdAt) > placeholderTTL {
			m.placeholders.Delete(key)
		}
		return true
	})
}

// runTTLJanitor periodically evicts stale typing and placeholder entries.
// This prevents memory accumulation when outbound paths fail to trigger
// preSend (e.g., LLM errors).
func (m *Manager) runTTLJanitor(ctx context.Context) {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.evictTypingEntries(now)
			m.evictPlaceholderEntries(now)
		}
	}
}
