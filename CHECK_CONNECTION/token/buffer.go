package token

import (
	"context"
	"log"
	"sync"
	"time"
)

// TokenBuffer implements a 2-layer buffer for continuous token supply
type TokenBuffer struct {
	apiClient *APIClient
	batchSize int

	active   []*TokenInfo
	activeMu sync.Mutex

	prefetch      []*TokenInfo
	prefetchMu    sync.Mutex
	prefetchReady chan struct{}

	threshold int
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

func NewTokenBuffer(apiClient *APIClient, batchSize int) *TokenBuffer {
	threshold := batchSize / 5
	if threshold < 5 {
		threshold = 5
	}

	return &TokenBuffer{
		apiClient:     apiClient,
		batchSize:     batchSize,
		active:        make([]*TokenInfo, 0, batchSize),
		prefetch:      make([]*TokenInfo, 0, batchSize),
		prefetchReady: make(chan struct{}, 1),
		threshold:     threshold,
		stopCh:        make(chan struct{}),
	}
}

func (b *TokenBuffer) Start() error {
	tokens, err := b.fetchBatch()
	if err != nil {
		return err
	}
	b.activeMu.Lock()
	b.active = tokens
	b.activeMu.Unlock()
	log.Printf("[BUFFER] Loaded %d tokens into active pool", len(tokens))

	b.wg.Add(1)
	go b.prefetchLoop()

	b.triggerPrefetch()

	return nil
}

func (b *TokenBuffer) AcquireToken() *TokenInfo {
	b.activeMu.Lock()

	if len(b.active) == 0 {
		b.activeMu.Unlock()
		b.trySwap()
		b.activeMu.Lock()
		if len(b.active) == 0 {
			b.activeMu.Unlock()
			return nil
		}
	}

	token := b.active[len(b.active)-1]
	b.active = b.active[:len(b.active)-1]
	remaining := len(b.active)
	b.activeMu.Unlock()

	if remaining < b.threshold {
		b.trySwap()
	}

	return token
}

func (b *TokenBuffer) ReportExhausted(ctx context.Context, email string) {
	go func() {
		if err := b.apiClient.ReportExhausted(ctx, email); err != nil {
			log.Printf("[BUFFER] Report exhausted failed for %s: %v", email, err)
		}
	}()
}

func (b *TokenBuffer) trySwap() {
	b.prefetchMu.Lock()
	if len(b.prefetch) == 0 {
		b.prefetchMu.Unlock()
		return
	}
	newActive := b.prefetch
	b.prefetch = make([]*TokenInfo, 0, b.batchSize)
	b.prefetchMu.Unlock()

	b.activeMu.Lock()
	b.active = append(newActive, b.active...)
	b.activeMu.Unlock()

	log.Printf("[BUFFER] Swapped in %d prefetched tokens", len(newActive))

	b.triggerPrefetch()
}

func (b *TokenBuffer) triggerPrefetch() {
	select {
	case b.prefetchReady <- struct{}{}:
	default:
	}
}

func (b *TokenBuffer) prefetchLoop() {
	defer b.wg.Done()

	for {
		select {
		case <-b.stopCh:
			return
		case <-b.prefetchReady:
			b.prefetchMu.Lock()
			needFetch := len(b.prefetch) == 0
			b.prefetchMu.Unlock()

			if !needFetch {
				continue
			}

			tokens, err := b.fetchBatch()
			if err != nil {
				log.Printf("[BUFFER] Prefetch failed: %v, retrying in 5s", err)
				select {
				case <-time.After(5 * time.Second):
				case <-b.stopCh:
					return
				}
				b.triggerPrefetch()
				continue
			}

			if len(tokens) == 0 {
				log.Printf("[BUFFER] No fresh tokens available, retrying in 10s")
				select {
				case <-time.After(10 * time.Second):
				case <-b.stopCh:
					return
				}
				b.triggerPrefetch()
				continue
			}

			b.prefetchMu.Lock()
			b.prefetch = tokens
			b.prefetchMu.Unlock()
			log.Printf("[BUFFER] Prefetched %d tokens", len(tokens))
		}
	}
}

func (b *TokenBuffer) fetchBatch() ([]*TokenInfo, error) {
	apiTokens, err := b.apiClient.FetchTokens(b.batchSize)
	if err != nil {
		return nil, err
	}

	tokens := make([]*TokenInfo, 0, len(apiTokens))
	for _, t := range apiTokens {
		tokens = append(tokens, &TokenInfo{
			Username:     t.Email,
			Password:     t.Password,
			RefreshToken: t.RefreshToken,
			TenantID:     t.TenantID,
		})
	}

	return tokens, nil
}

func (b *TokenBuffer) Stop() {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.wg.Wait()
}

func (b *TokenBuffer) Stats() (active, prefetch int) {
	b.activeMu.Lock()
	active = len(b.active)
	b.activeMu.Unlock()

	b.prefetchMu.Lock()
	prefetch = len(b.prefetch)
	b.prefetchMu.Unlock()

	return
}
