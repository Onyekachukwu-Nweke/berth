package http

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestOperation_ReconnectingNeverRepeatsTheMutation is the reason operations
// exist at all.
//
// An operation's log is delivered over SSE, and browsers reconnect an EventSource
// automatically whenever the connection drops. If the mutation were driven by
// that GET — as the older streaming endpoints do — every reconnect would run the
// rollback again. Watching is therefore separated from starting: the POST runs
// the work once, and any number of readers replay it.
func TestOperation_ReconnectingNeverRepeatsTheMutation(t *testing.T) {
	s := newOperationStore()
	var runs int32

	id := s.start(func(w opWriter) error {
		atomic.AddInt32(&runs, 1)
		_, _ = w.Write([]byte("working\n"))
		return nil
	})

	op, ok := s.get(id)
	if !ok {
		t.Fatal("operation not found")
	}
	<-op.done

	// Read it several times, as a reconnecting client would.
	for i := 0; i < 3; i++ {
		var out strings.Builder
		if err := op.stream(context.Background(), &out); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		if !strings.Contains(out.String(), "working") {
			t.Errorf("read %d replayed nothing: %q", i, out.String())
		}
	}
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Errorf("the operation ran %d times; watching must never re-run it", got)
	}
}

// TestOperation_ReplaysFromTheStart is what makes a reconnect useful rather than
// merely harmless: a client that joins late still sees everything that happened.
func TestOperation_ReplaysFromTheStart(t *testing.T) {
	s := newOperationStore()
	release := make(chan struct{})
	id := s.start(func(w opWriter) error {
		_, _ = w.Write([]byte("first\n"))
		<-release
		_, _ = w.Write([]byte("second\n"))
		return nil
	})
	op, _ := s.get(id)

	// Join after the first line, then let the operation finish while streaming.
	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		_ = op.stream(context.Background(), &out)
		done <- out.String()
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case got := <-done:
		if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
			t.Errorf("late reader missed output: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not finish")
	}
}

// TestOperation_KeepsRunningWhenTheReaderLeaves covers a closed browser tab: the
// rollback must complete regardless, because abandoning a half-applied activation
// is far worse than finishing one nobody is watching.
func TestOperation_KeepsRunningWhenTheReaderLeaves(t *testing.T) {
	s := newOperationStore()
	finished := make(chan struct{})
	started := make(chan struct{})
	id := s.start(func(w opWriter) error {
		close(started)
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte("done\n"))
		close(finished)
		return nil
	})
	op, _ := s.get(id)
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the reader disconnects immediately
	var out strings.Builder
	if err := op.stream(ctx, &out); err != nil {
		t.Errorf("a disconnected reader should not be an error: %v", err)
	}

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("operation stopped when its reader left")
	}
}

// TestOperation_SurfacesTheFailure keeps a failed rollback from reading as a
// successful one.
func TestOperation_SurfacesTheFailure(t *testing.T) {
	s := newOperationStore()
	id := s.start(func(w opWriter) error {
		_, _ = w.Write([]byte("trying\n"))
		return context.DeadlineExceeded
	})
	op, _ := s.get(id)
	<-op.done

	var out strings.Builder
	if err := op.stream(context.Background(), &out); err == nil {
		t.Error("stream should return the operation's error")
	}
	if !strings.Contains(out.String(), "trying") {
		t.Errorf("output before the failure should still be replayed: %q", out.String())
	}
}

func TestOperation_IDsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newOperationID()
		if seen[id] {
			t.Fatalf("duplicate operation id %q", id)
		}
		seen[id] = true
	}
}
