package writer

import (
	"bufio"
	"fmt"
	"linkedin_fetcher/models"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Batch size before auto-flush
	BatchSize = 100
	// Max time between flushes
	FlushInterval = 2 * time.Second
	// Write buffer size - 1MB
	WriteBufferSize = 1024 * 1024
)

// ResultWriter handles high-performance writing of results to file
type ResultWriter struct {
	filepath string
	file     *os.File
	writer   *bufio.Writer
	mu       sync.Mutex
	count    int64
	pending  int32 // pending writes since last flush

	// Async flush
	flushChan chan struct{}
	closeChan chan struct{}
	closeOnce sync.Once
}

// NewResultWriter creates a new result writer with async flushing
func NewResultWriter(filepath string) (*ResultWriter, error) {
	file, err := os.OpenFile(filepath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	w := &ResultWriter{
		filepath:  filepath,
		file:      file,
		writer:    bufio.NewWriterSize(file, WriteBufferSize),
		flushChan: make(chan struct{}, 1),
		closeChan: make(chan struct{}),
	}

	// Start background flusher
	go w.backgroundFlusher()

	return w, nil
}

// backgroundFlusher periodically flushes the buffer
func (w *ResultWriter) backgroundFlusher() {
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.flush()
		case <-w.flushChan:
			w.flush()
		case <-w.closeChan:
			return
		}
	}
}

// flush performs the actual flush operation
func (w *ResultWriter) flush() {
	w.mu.Lock()
	if w.writer.Buffered() > 0 {
		w.writer.Flush()
	}
	atomic.StoreInt32(&w.pending, 0)
	w.mu.Unlock()
}

// triggerFlush signals the background flusher (non-blocking)
func (w *ResultWriter) triggerFlush() {
	select {
	case w.flushChan <- struct{}{}:
	default:
		// Already triggered, skip
	}
}

// WriteResult writes a single result to buffer (thread-safe, batched)
func (w *ResultWriter) WriteResult(result *models.Result) error {
	w.mu.Lock()

	// Format directly to buffer - avoid intermediate string allocation
	_, err := fmt.Fprintf(w.writer, "%s|%s|%d|%s|%s\n",
		result.Email,
		result.DisplayName,
		result.ConnectionCount,
		result.Location,
		result.LinkedInURL,
	)

	if err != nil {
		w.mu.Unlock()
		return err
	}

	atomic.AddInt64(&w.count, 1)
	pending := atomic.AddInt32(&w.pending, 1)
	w.mu.Unlock()

	// Trigger flush if batch size reached
	if pending >= BatchSize {
		w.triggerFlush()
	}

	return nil
}

// WriteBatch writes multiple results at once (more efficient)
func (w *ResultWriter) WriteBatch(results []*models.Result) error {
	if len(results) == 0 {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, result := range results {
		_, err := fmt.Fprintf(w.writer, "%s|%s|%d|%s|%s\n",
			result.Email,
			result.DisplayName,
			result.ConnectionCount,
			result.Location,
			result.LinkedInURL,
		)
		if err != nil {
			return err
		}
		atomic.AddInt64(&w.count, 1)
	}

	// Flush after batch
	return w.writer.Flush()
}

// Close flushes and closes the writer
func (w *ResultWriter) Close() error {
	var closeErr error

	w.closeOnce.Do(func() {
		// Signal background flusher to stop
		close(w.closeChan)

		// Final flush
		w.mu.Lock()
		if err := w.writer.Flush(); err != nil {
			closeErr = err
		}
		if err := w.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		w.mu.Unlock()
	})

	return closeErr
}

// Count returns the number of results written
func (w *ResultWriter) Count() int {
	return int(atomic.LoadInt64(&w.count))
}

// Flush forces an immediate flush
func (w *ResultWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Flush()
}
