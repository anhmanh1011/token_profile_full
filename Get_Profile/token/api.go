package token

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	apiBatchDeleteSize = 20
	apiFlushInterval   = 5 * time.Second
)

// APIClient is an HTTP client for the Python token-management API service.
// It fetches the next available token and batches delete requests for consumed accounts.
type APIClient struct {
	baseURL    string
	httpClient *http.Client
	deleteChan chan string
}

// fetchTokenResponse is a single token in the batch response.
type fetchTokenResponse struct {
	Email        string `json:"email"`
	RefreshToken string `json:"refresh_token"`
	TenantID     string `json:"tenant_id"`
}

// fetchBatchResponse is the JSON payload returned by GET /tokens/next?count=N.
type fetchBatchResponse struct {
	Tokens []fetchTokenResponse `json:"tokens"`
	Count  int                  `json:"count"`
}

// deleteRequest is the JSON payload sent to POST /users/delete.
type deleteRequest struct {
	Emails []string `json:"emails"`
}

// NewAPIClient creates an APIClient that talks to the Python service at baseURL.
// The HTTP client has a 30-second timeout; deleteChan is buffered to 1000.
func NewAPIClient(baseURL string) *APIClient {
	return &APIClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		deleteChan: make(chan string, 1000),
	}
}

// FetchTokens calls GET {baseURL}/tokens/next?count=N and returns up to N tokens.
// Returns nil (empty slice) when the queue is empty (202); caller should retry.
// Connection errors are retried up to 3 times with a 5-second pause.
func (c *APIClient) FetchTokens(count int) ([]*TokenInfo, error) {
	url := fmt.Sprintf("%s/tokens/next?count=%d", c.baseURL, count)

	var lastErr error

	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(5 * time.Second)
		}

		resp, err := c.httpClient.Get(url) //nolint:noctx
		if err != nil {
			lastErr = fmt.Errorf("token/api: fetch attempt %d: %w", attempt+1, err)
			slog.Warn("token/api: fetch connection error, retrying",
				"attempt", attempt+1,
				"err", err,
			)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			lastErr = fmt.Errorf("token/api: read response body: %w", readErr)
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			var batch fetchBatchResponse
			if err := json.Unmarshal(body, &batch); err != nil {
				return nil, fmt.Errorf("token/api: parse batch response: %w", err)
			}
			tokens := make([]*TokenInfo, 0, len(batch.Tokens))
			for _, t := range batch.Tokens {
				tokens = append(tokens, &TokenInfo{
					Username:     t.Email,
					RefreshToken: t.RefreshToken,
					TenantID:     t.TenantID,
				})
			}
			return tokens, nil

		case http.StatusAccepted:
			return nil, nil // Queue empty

		default:
			return nil, fmt.Errorf("token/api: unexpected status %d", resp.StatusCode)
		}
	}

	return nil, lastErr
}

// QueueDelete enqueues email for batch deletion. The send is non-blocking;
// if the channel is full the email is dropped and a warning is logged.
func (c *APIClient) QueueDelete(email string) {
	select {
	case c.deleteChan <- email:
	default:
		slog.Warn("token/api: delete channel full, dropping email", "email", email)
	}
}

// StartDeleteWorker launches a background goroutine that drains deleteChan,
// batches emails (flush at apiBatchDeleteSize items or every apiFlushInterval),
// and calls sendDeleteBatch. On ctx cancellation the worker drains any remaining
// emails and flushes the final batch before returning.
//
// The caller MUST call wg.Add(1) before invoking this method.
func (c *APIClient) StartDeleteWorker(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		defer wg.Done()

		batch := make([]string, 0, apiBatchDeleteSize)
		ticker := time.NewTicker(apiFlushInterval)
		defer ticker.Stop()

		flush := func() {
			if len(batch) == 0 {
				return
			}
			c.sendDeleteBatch(batch)
			batch = batch[:0]
		}

		for {
			select {
			case email, ok := <-c.deleteChan:
				if !ok {
					// Channel closed — flush and exit.
					flush()
					return
				}
				batch = append(batch, email)
				if len(batch) >= apiBatchDeleteSize {
					flush()
				}

			case <-ticker.C:
				flush()

			case <-ctx.Done():
				// Drain remaining emails before exiting.
				for {
					select {
					case email, ok := <-c.deleteChan:
						if !ok {
							flush()
							return
						}
						batch = append(batch, email)
						if len(batch) >= apiBatchDeleteSize {
							flush()
						}
					default:
						flush()
						return
					}
				}
			}
		}
	}()
}

// CloseDeleteChan closes the underlying delete channel, signalling
// StartDeleteWorker to drain and exit.
func (c *APIClient) CloseDeleteChan() {
	close(c.deleteChan)
}

// sendDeleteBatch posts a batch of email addresses to POST {baseURL}/users/delete.
// Errors are logged; the function is best-effort and does not return an error.
func (c *APIClient) sendDeleteBatch(emails []string) {
	payload, err := json.Marshal(deleteRequest{Emails: emails})
	if err != nil {
		slog.Error("token/api: failed to marshal delete request",
			"count", len(emails),
			"err", err,
		)
		return
	}

	url := c.baseURL + "/users/delete"
	resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(payload)) //nolint:noctx // best-effort background flush
	if err != nil {
		slog.Error("token/api: failed to send delete batch",
			"count", len(emails),
			"emails", joinQuoted(emails),
			"err", err,
		)
		return
	}
	defer resp.Body.Close()

	// Drain body to allow connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= http.StatusBadRequest {
		slog.Error("token/api: delete batch returned error status",
			"status", resp.StatusCode,
			"count", len(emails),
		)
		return
	}

	slog.Info("token/api: delete batch sent", "count", len(emails))
}

// joinQuoted returns the strings joined as `"a","b","c"` for log output.
func joinQuoted(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	var b strings.Builder
	// Pre-grow: each element is at most len(s)+3 bytes ("x",)
	for _, s := range ss {
		b.Grow(len(s) + 3)
	}
	for i, s := range ss {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(s)
		b.WriteByte('"')
	}
	return b.String()
}
