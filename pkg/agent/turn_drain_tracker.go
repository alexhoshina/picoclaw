package agent

import "sync"

type turnDrainTracker struct {
	mu           sync.Mutex
	cond         *sync.Cond
	currentEpoch uint64
	counts       map[uint64]int
}

func newTurnDrainTracker() *turnDrainTracker {
	tracker := &turnDrainTracker{
		counts: make(map[uint64]int),
	}
	tracker.cond = sync.NewCond(&tracker.mu)
	return tracker
}

func (t *turnDrainTracker) beginTurn() uint64 {
	if t == nil {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	epoch := t.currentEpoch
	t.counts[epoch]++
	return epoch
}

func (t *turnDrainTracker) endTurn(epoch uint64) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	count, ok := t.counts[epoch]
	if !ok {
		return
	}
	if count <= 1 {
		delete(t.counts, epoch)
	} else {
		t.counts[epoch] = count - 1
	}
	t.cond.Broadcast()
}

func (t *turnDrainTracker) rotateEpoch() uint64 {
	if t == nil {
		return 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	epoch := t.currentEpoch
	t.currentEpoch++
	return epoch
}

func (t *turnDrainTracker) waitForEpoch(epoch uint64) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	for t.counts[epoch] > 0 {
		t.cond.Wait()
	}
}
