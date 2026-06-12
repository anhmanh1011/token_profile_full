package worker

import (
	"errors"
	"linkedin_fetcher/models"
	"linkedin_fetcher/progress"
	"linkedin_fetcher/reader"
	"linkedin_fetcher/token"
	"linkedin_fetcher/writer"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type sequenceProfileClient struct {
	mu       sync.Mutex
	statuses []int
	calls    int
}

func (c *sequenceProfileClient) FetchProfile(email, accessToken string) (*models.Result, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls++
	status := 200
	if c.calls <= len(c.statuses) {
		status = c.statuses[c.calls-1]
	}
	if status == 200 {
		return nil, status, nil
	}
	return nil, status, errors.New("api error")
}

func (c *sequenceProfileClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestWorkerRetriesCurrentJobAfterTokenDeath(t *testing.T) {
	dir := t.TempDir()
	emailsPath := filepath.Join(dir, "emails.txt")
	if err := os.WriteFile(emailsPath, []byte("a@example.com\n"), 0644); err != nil {
		t.Fatalf("write emails file: %v", err)
	}

	bitmap, err := progress.LoadOrCreate(filepath.Join(dir, "emails.ckpt"), emailsPath)
	if err != nil {
		t.Fatalf("load bitmap: %v", err)
	}

	resultWriter, err := writer.NewResultWriter(filepath.Join(dir, "result.txt"))
	if err != nil {
		t.Fatalf("new result writer: %v", err)
	}
	defer resultWriter.Close()

	manager := token.NewManager()
	manager.InitEmptyQueue(2)
	manager.SetDeadChan(make(chan string, 2))
	manager.AddToken(&token.TokenInfo{
		Username:    "dead@example.com",
		AccessToken: "dead-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	manager.AddToken(&token.TokenInfo{
		Username:    "ok@example.com",
		AccessToken: "ok-token",
		ExpiresAt:   time.Now().Add(time.Hour),
	})

	client := &sequenceProfileClient{statuses: []int{401, 200}}
	pool := NewPool(1, client, manager, resultWriter, 1, 0, bitmap)
	pool.Start()
	if !pool.Submit(reader.EmailJob{Email: "a@example.com", LineIdx: 0}) {
		t.Fatal("submit failed")
	}
	pool.Close()

	if got := client.Calls(); got != 2 {
		t.Fatalf("FetchProfile calls = %d, want 2", got)
	}
	processed, successful, failed, _ := pool.Stats()
	if processed != 1 || successful != 1 || failed != 0 {
		t.Fatalf("stats = processed:%d successful:%d failed:%d, want 1/1/0", processed, successful, failed)
	}
	if got := bitmap.Done(); got != 1 {
		t.Fatalf("bitmap done = %d, want 1", got)
	}
	_, alive, dead := manager.Stats()
	if alive != 1 || dead != 1 {
		t.Fatalf("token stats alive/dead = %d/%d, want 1/1", alive, dead)
	}
}
