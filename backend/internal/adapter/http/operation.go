package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"sync"
	"time"
)

// operationRetention is how long a finished operation stays readable. Long enough
// for an operator to reconnect and read what happened, short enough that a
// long-lived process does not accumulate logs forever.
const operationRetention = 30 * time.Minute

// operation is one mutating action running in the background, with its output
// buffered so it can be replayed.
//
// The buffer is the point. An operation's log is delivered over SSE, and browsers
// reconnect an EventSource automatically whenever the connection drops. If the
// mutation were driven by that GET, every reconnect would run the rollback again.
// Separating "start the operation" (POST, once) from "watch it" (GET, replayable)
// removes that hazard entirely.
type operation struct {
	mu       sync.Mutex
	buf      []byte
	done     chan struct{}
	err      error
	finished time.Time
	// waiters are notified when new output arrives, so a reader can block rather
	// than poll.
	waiters []chan struct{}
}

// opWriter is what the use case writes progress to.
type opWriter = io.Writer

func (o *operation) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.buf = append(o.buf, p...)
	for _, ch := range o.waiters {
		close(ch)
	}
	o.waiters = nil
	o.mu.Unlock()
	return len(p), nil
}

// notify returns a channel closed on the next write, so stream can wait without
// spinning.
func (o *operation) notify() chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	ch := make(chan struct{})
	o.waiters = append(o.waiters, ch)
	return ch
}

// stream replays everything written so far, then follows until the operation
// finishes or the caller disconnects. Replaying from the start is what makes a
// reconnect useful rather than merely harmless.
func (o *operation) stream(ctx context.Context, out io.Writer) error {
	sent := 0
	for {
		o.mu.Lock()
		chunk := append([]byte(nil), o.buf[sent:]...)
		sent = len(o.buf)
		o.mu.Unlock()

		if len(chunk) > 0 {
			if _, err := out.Write(chunk); err != nil {
				return err
			}
		}

		select {
		case <-o.done:
			// Drain anything written between the copy above and completion.
			o.mu.Lock()
			rest := append([]byte(nil), o.buf[sent:]...)
			err := o.err
			o.mu.Unlock()
			if len(rest) > 0 {
				if _, werr := out.Write(rest); werr != nil {
					return werr
				}
			}
			return err
		case <-ctx.Done():
			// The reader went away; the operation keeps running.
			return nil
		case <-o.notify():
		}
	}
}

// operationStore holds in-flight and recently finished operations.
//
// In memory on purpose: berth is a local, single-user process, and an
// operation that did not survive a restart is one whose outcome is on the server
// anyway — the release state document, not this store, is the record of what
// happened.
type operationStore struct {
	mu  sync.Mutex
	ops map[string]*operation
}

func newOperationStore() *operationStore {
	return &operationStore{ops: map[string]*operation{}}
}

// start runs fn in the background and returns the id to watch it by.
func (s *operationStore) start(fn func(opWriter) error) string {
	op := &operation{done: make(chan struct{})}
	id := newOperationID()

	s.mu.Lock()
	s.sweepLocked()
	s.ops[id] = op
	s.mu.Unlock()

	go func() {
		err := fn(op)
		op.mu.Lock()
		op.err = err
		op.finished = time.Now()
		op.mu.Unlock()
		close(op.done)
	}()
	return id
}

func (s *operationStore) get(id string) (*operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	return op, ok
}

// sweepLocked drops operations that finished long enough ago to be uninteresting.
func (s *operationStore) sweepLocked() {
	cutoff := time.Now().Add(-operationRetention)
	for id, op := range s.ops {
		op.mu.Lock()
		finished := op.finished
		op.mu.Unlock()
		if !finished.IsZero() && finished.Before(cutoff) {
			delete(s.ops, id)
		}
	}
}

func newOperationID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A collision here would only mix up two operation logs, and the fallback
		// still separates them by time.
		return "op-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "op-" + hex.EncodeToString(b[:])
}
