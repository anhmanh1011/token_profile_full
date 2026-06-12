package token

import (
	"context"
	"testing"
	"time"
)

func TestAcquireTokenReturnsNilWhenContextCancelled(t *testing.T) {
	manager := NewManager()
	manager.InitEmptyQueue(1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := manager.AcquireToken(ctx); got != nil {
		t.Fatalf("AcquireToken returned %#v after context cancellation, want nil", got)
	}
}

func TestCloseQueueIsSafeWithConcurrentQueueOperations(t *testing.T) {
	manager := NewManager()
	manager.InitEmptyQueue(1)
	manager.SetDeadChan(make(chan string, 1))

	manager.AddToken(&TokenInfo{
		Username:    "first@example.com",
		AccessToken: "first",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	acquired := manager.AcquireToken(context.Background())
	if acquired == nil {
		t.Fatal("expected token")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.CloseQueue()
		manager.AddToken(&TokenInfo{
			Username:    "second@example.com",
			AccessToken: "second",
			ExpiresAt:   time.Now().Add(time.Hour),
		})
		manager.ReleaseToken(acquired)
		manager.MarkDeadAndRelease(acquired)
		manager.CloseQueue()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queue operations did not finish")
	}
}
