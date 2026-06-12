package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSendDeleteBatchRetriesTransientFailure(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/delete" {
			t.Fatalf("path = %s, want /users/delete", r.URL.Path)
		}

		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()

		if current == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewAPIClient(server.URL)
	if err := client.sendDeleteBatch(context.Background(), []string{"a@example.com"}); err != nil {
		t.Fatalf("sendDeleteBatch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDeleteWorkerFlushesQueuedEmails(t *testing.T) {
	got := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req deleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got <- req.Emails
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewAPIClient(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	client.StartDeleteWorker(ctx, &wg)
	client.QueueDelete("a@example.com")
	client.CloseDeleteChan()
	wg.Wait()

	select {
	case emails := <-got:
		if len(emails) != 1 || emails[0] != "a@example.com" {
			t.Fatalf("emails = %#v, want [a@example.com]", emails)
		}
	default:
		t.Fatal("delete worker did not send batch")
	}
}
