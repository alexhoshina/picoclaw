// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package channels

import (
	"context"
	"math"

	"golang.org/x/time/rate"

	"github.com/sipeed/picoclaw/pkg/bus"
)

type channelWorker struct {
	ch         Channel
	queue      chan bus.OutboundMessage
	mediaQueue chan bus.OutboundMediaMessage
	done       chan struct{}
	mediaDone  chan struct{}
	limiter    *rate.Limiter
}

// newChannelWorker creates a channelWorker with a rate limiter configured
// for the given channel name.
func newChannelWorker(name string, ch Channel) *channelWorker {
	rateVal := float64(defaultRateLimit)
	if r, ok := channelRateConfig[name]; ok {
		rateVal = r
	}
	burst := int(math.Max(1, math.Ceil(rateVal/2)))

	return &channelWorker{
		ch:         ch,
		queue:      make(chan bus.OutboundMessage, defaultChannelQueueSize),
		mediaQueue: make(chan bus.OutboundMediaMessage, defaultChannelQueueSize),
		done:       make(chan struct{}),
		mediaDone:  make(chan struct{}),
		limiter:    rate.NewLimiter(rate.Limit(rateVal), burst),
	}
}

// sendChunkedMessage splits and sends a message if it exceeds max length.
func (m *Manager) sendChunkedMessage(ctx context.Context, name string, w *channelWorker, msg bus.OutboundMessage) {
	maxLen := 0
	if mlp, ok := w.ch.(MessageLengthProvider); ok {
		maxLen = mlp.MaxMessageLength()
	}

	if maxLen > 0 && len([]rune(msg.Content)) > maxLen {
		chunks := SplitMessage(msg.Content, maxLen)
		for _, chunk := range chunks {
			chunkMsg := msg
			chunkMsg.Content = chunk
			m.sendWithRetry(ctx, name, w, chunkMsg)
		}
	} else {
		m.sendWithRetry(ctx, name, w, msg)
	}
}

// runWorker processes outbound messages for a single channel.
func (m *Manager) runWorker(ctx context.Context, name string, w *channelWorker) {
	defer close(w.done)
	for {
		select {
		case msg, ok := <-w.queue:
			if !ok {
				return
			}
			m.sendChunkedMessage(ctx, name, w, msg)
		case <-ctx.Done():
			return
		}
	}
}

// runMediaWorker processes outbound media messages for a single channel.
func (m *Manager) runMediaWorker(ctx context.Context, name string, w *channelWorker) {
	defer close(w.mediaDone)
	for {
		select {
		case msg, ok := <-w.mediaQueue:
			if !ok {
				return
			}
			m.sendMediaWithRetry(ctx, name, w, msg)
		case <-ctx.Done():
			return
		}
	}
}
