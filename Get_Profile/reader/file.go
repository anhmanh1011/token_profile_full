package reader

import (
	"bufio"
	"os"
	"strings"
	"sync/atomic"
)

// EmailReader handles optimized reading of large email files
type EmailReader struct {
	filepath   string
	bufferSize int
	totalCount int64 // Counted during read
}

// NewEmailReader creates a new email reader
func NewEmailReader(filepath string, bufferSize int) *EmailReader {
	return &EmailReader{
		filepath:   filepath,
		bufferSize: bufferSize,
	}
}

// ReadEmails streams emails from file to a channel (memory efficient)
// Returns channel, error channel, and pointer to count (updated during read)
func (r *EmailReader) ReadEmailsWithCount() (<-chan string, <-chan error, *int64) {
	// Large buffer for high throughput - 100K items
	emailChan := make(chan string, 100000)
	errChan := make(chan error, 1)

	go func() {
		defer close(emailChan)
		defer close(errChan)

		file, err := os.Open(r.filepath)
		if err != nil {
			errChan <- err
			return
		}
		defer file.Close()

		// Use larger buffer for faster IO - 4MB
		reader := bufio.NewReaderSize(file, 4*1024*1024)
		scanner := bufio.NewScanner(reader)

		// Set max token size for large lines
		buf := make([]byte, r.bufferSize)
		scanner.Buffer(buf, r.bufferSize)

		var count int64
		for scanner.Scan() {
			line := scanner.Bytes()
			// Fast trim - avoid string conversion until needed
			start, end := 0, len(line)
			for start < end && (line[start] == ' ' || line[start] == '\t' || line[start] == '\r' || line[start] == '\n') {
				start++
			}
			for end > start && (line[end-1] == ' ' || line[end-1] == '\t' || line[end-1] == '\r' || line[end-1] == '\n') {
				end--
			}

			if start < end {
				email := string(line[start:end])
				if isValidEmailFast(email) {
					count++
					emailChan <- email
				}
			}
		}

		atomic.StoreInt64(&r.totalCount, count)

		if err := scanner.Err(); err != nil {
			errChan <- err
		}
	}()

	return emailChan, errChan, &r.totalCount
}

// ReadEmails for backward compatibility
func (r *EmailReader) ReadEmails() (<-chan string, <-chan error) {
	ch, errCh, _ := r.ReadEmailsWithCount()
	return ch, errCh
}

// GetTotalCount returns the total count (available after read completes)
func (r *EmailReader) GetTotalCount() int64 {
	return atomic.LoadInt64(&r.totalCount)
}

// CountEmails counts total emails in file using fast line counting
func (r *EmailReader) CountEmails() (int, error) {
	file, err := os.Open(r.filepath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	// Fast counting with large buffer
	reader := bufio.NewReaderSize(file, 4*1024*1024)
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, r.bufferSize)
	scanner.Buffer(buf, r.bufferSize)

	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 0 && containsAt(line) {
			count++
		}
	}

	return count, scanner.Err()
}

// containsAt fast check for @ symbol
func containsAt(b []byte) bool {
	for _, c := range b {
		if c == '@' {
			return true
		}
	}
	return false
}

// isValidEmailFast performs fast email validation
func isValidEmailFast(email string) bool {
	n := len(email)
	if n < 5 { // minimum: a@b.c
		return false
	}

	atIdx := -1
	dotAfterAt := false

	for i := 0; i < n; i++ {
		if email[i] == '@' {
			if atIdx != -1 || i == 0 {
				return false // multiple @ or @ at start
			}
			atIdx = i
		} else if atIdx != -1 && email[i] == '.' {
			dotAfterAt = true
		}
	}

	return atIdx > 0 && atIdx < n-1 && dotAfterAt
}

// isValidEmail for backward compatibility
func isValidEmail(email string) bool {
	atIdx := strings.Index(email, "@")
	if atIdx <= 0 || atIdx >= len(email)-1 {
		return false
	}
	dotIdx := strings.LastIndex(email[atIdx:], ".")
	return dotIdx > 0
}
