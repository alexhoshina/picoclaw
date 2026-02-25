// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package channels

import (
	"context"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// typingEntry wraps a typing stop function with a creation timestamp for TTL eviction.
type typingEntry struct {
	stop      func()
	createdAt time.Time
}

// placeholderEntry wraps a placeholder ID with a creation timestamp for TTL eviction.
type placeholderEntry struct {
	id        string
	createdAt time.Time
}

// stopTypingIfNeeded stops any pending typing indicator for the given key.
// This is idempotent and safe to call multiple times.
func (m *Manager) stopTypingIfNeeded(key string) {
	v, loaded := m.typingStops.LoadAndDelete(key)
	if !loaded {
		return
	}
	entry, ok := v.(typingEntry)
	if !ok {
		return
	}
	entry.stop() // idempotent, safe to call multiple times
}

// tryEditPlaceholder attempts to edit a placeholder message.
// Returns true if successfully edited (caller should skip Send).
func (m *Manager) tryEditPlaceholder(ctx context.Context, key string, msg bus.OutboundMessage, ch Channel) bool {
	v, loaded := m.placeholders.LoadAndDelete(key)
	if !loaded {
		return false
	}
	entry, ok := v.(placeholderEntry)
	if !ok || entry.id == "" {
		return false
	}
	editor, ok := ch.(MessageEditor)
	if !ok {
		return false
	}
	if err := editor.EditMessage(ctx, msg.ChatID, entry.id, msg.Content); err != nil {
		return false // edit failed, fall through to Send
	}
	return true // edited successfully
}

// preSend handles typing stop and placeholder editing before sending a message.
// Returns true if the message was edited into a placeholder (skip Send).
func (m *Manager) preSend(ctx context.Context, name string, msg bus.OutboundMessage, ch Channel) bool {
	key := name + ":" + msg.ChatID
	m.stopTypingIfNeeded(key)
	return m.tryEditPlaceholder(ctx, key, msg, ch)
}
