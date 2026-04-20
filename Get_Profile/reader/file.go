package reader

import (
	"bufio"
	"linkedin_fetcher/progress"
	"os"
	"strings"
	"sync/atomic"
)

// EmailJob pairs an email with its line index in the source file. The
// LineIdx is the 0-based position in emails.txt (every physical line,
// including blanks/invalid — option A), used by the bitmap to track
// terminal processing.
type EmailJob struct {
	Email   string
	LineIdx int64
}

// EmailReader handles optimized reading of large email files
type EmailReader struct {
	filepath   string
	bufferSize int
	bitmap     *progress.Bitmap

	totalCount int64 // emails actually pushed to channel
	skipCount  int64 // lines skipped because bitmap bit set
}

// NewEmailReader creates a new email reader. bitmap may be nil to disable
// skip/tracking (useful for ad-hoc runs without a checkpoint).
func NewEmailReader(filepath string, bufferSize int, bitmap *progress.Bitmap) *EmailReader {
	return &EmailReader{
		filepath:   filepath,
		bufferSize: bufferSize,
		bitmap:     bitmap,
	}
}

// ReadJobsInto streams EmailJobs from file, skipping lines already set in the
// bitmap, and invokes submit for each job in the same goroutine as the caller.
// submit returns false if the downstream pool is shutting down, in which case
// ReadJobsInto stops. Runs synchronously — caller owns the goroutine.
func (r *EmailReader) ReadJobsInto(submit func(EmailJob) bool) error {
	file, err := os.Open(r.filepath)
	if err != nil {
		return err
	}
	defer file.Close()

	rd := bufio.NewReaderSize(file, 4*1024*1024)
	scanner := bufio.NewScanner(rd)
	buf := make([]byte, r.bufferSize)
	scanner.Buffer(buf, r.bufferSize)

	var idx int64 = 0
	for scanner.Scan() {
		line := scanner.Bytes()
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
				if r.bitmap != nil && r.bitmap.Test(idx) {
					atomic.AddInt64(&r.skipCount, 1)
				} else {
					atomic.AddInt64(&r.totalCount, 1)
					if !submit(EmailJob{Email: email, LineIdx: idx}) {
						// Pool shutting down, stop reading.
						return nil
					}
				}
			}
		}
		idx++ // option A: increment for every physical line
	}

	return scanner.Err()
}

// GetTotalCount returns the number of emails pushed to channel (excludes skipped).
func (r *EmailReader) GetTotalCount() int64 {
	return atomic.LoadInt64(&r.totalCount)
}

// GetSkipCount returns the number of lines skipped via bitmap match.
func (r *EmailReader) GetSkipCount() int64 {
	return atomic.LoadInt64(&r.skipCount)
}

// isValidEmailFast performs fast email validation
func isValidEmailFast(email string) bool {
	n := len(email)
	if n < 5 {
		return false
	}

	atIdx := -1
	dotAfterAt := false

	for i := 0; i < n; i++ {
		if email[i] == '@' {
			if atIdx != -1 || i == 0 {
				return false
			}
			atIdx = i
		} else if atIdx != -1 && email[i] == '.' {
			dotAfterAt = true
		}
	}

	return atIdx > 0 && atIdx < n-1 && dotAfterAt
}

// isValidEmail kept for backward compatibility.
func isValidEmail(email string) bool {
	atIdx := strings.Index(email, "@")
	if atIdx <= 0 || atIdx >= len(email)-1 {
		return false
	}
	dotIdx := strings.LastIndex(email[atIdx:], ".")
	return dotIdx > 0
}
