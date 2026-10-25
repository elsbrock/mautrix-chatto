package connector

import (
	"context"
	"sync"
	"time"
)

// echoTracker suppresses realtime echoes of messages the bridge itself sent.
//
// bridgev2 already ignores remote messages whose ID is in the database, but
// Chatto can deliver message_posted over the WebSocket before CreateMessage
// returns, i.e. before the ID is known or saved. While a send to a room is in
// flight, echoes from our own account in that room wait for it to finish and
// are then checked against the IDs the sends returned.
type echoTracker struct {
	mu       sync.Mutex
	inflight map[string]int           // room ID → sends in flight
	done     map[string]chan struct{} // room ID → closed when inflight drops to 0
	sent     map[string]time.Time     // message ID → when it was sent
	// WaitTimeout bounds how long an echo waits for in-flight sends.
	WaitTimeout time.Duration
	// TTL is how long sent IDs are remembered.
	TTL time.Duration
}

func newEchoTracker() *echoTracker {
	return &echoTracker{
		inflight:    make(map[string]int),
		done:        make(map[string]chan struct{}),
		sent:        make(map[string]time.Time),
		WaitTimeout: 15 * time.Second,
		TTL:         10 * time.Minute,
	}
}

// Begin registers a send to roomID. The returned function must be called
// with the resulting message ID (empty on failure) when the send finishes.
func (t *echoTracker) Begin(roomID string) (finish func(messageID string)) {
	t.mu.Lock()
	t.inflight[roomID]++
	if _, ok := t.done[roomID]; !ok {
		t.done[roomID] = make(chan struct{})
	}
	t.mu.Unlock()
	var once sync.Once
	return func(messageID string) {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if messageID != "" {
				t.sent[messageID] = time.Now()
			}
			t.inflight[roomID]--
			if t.inflight[roomID] <= 0 {
				delete(t.inflight, roomID)
				if ch, ok := t.done[roomID]; ok {
					close(ch)
					delete(t.done, roomID)
				}
			}
			t.gcLocked()
		})
	}
}

// MarkSent records an ID as sent by the bridge without a Begin call, e.g.
// for edits or reactions that are known immediately.
func (t *echoTracker) MarkSent(messageID string) {
	t.mu.Lock()
	t.sent[messageID] = time.Now()
	t.mu.Unlock()
}

// IsEcho reports whether messageID, posted by our own account in roomID, was
// sent by the bridge. It may block while sends to the room are in flight.
func (t *echoTracker) IsEcho(ctx context.Context, roomID, messageID string) bool {
	t.mu.Lock()
	if _, ok := t.sent[messageID]; ok {
		t.mu.Unlock()
		return true
	}
	ch, waiting := t.done[roomID]
	t.mu.Unlock()
	if waiting {
		timer := time.NewTimer(t.WaitTimeout)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.sent[messageID]
	return ok
}

func (t *echoTracker) gcLocked() {
	cutoff := time.Now().Add(-t.TTL)
	for id, at := range t.sent {
		if at.Before(cutoff) {
			delete(t.sent, id)
		}
	}
}
