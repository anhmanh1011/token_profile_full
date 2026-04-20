// Package progress provides crash-recovery tracking for line-indexed email
// processing via an on-disk bitmap. One bit per line in emails.txt — set
// after a terminal outcome so the next run skips already-processed lines.
package progress

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	magic      = "BMP1"
	version    = byte(1)
	headerSize = 64
	fpSampleSize = 64 * 1024 // bytes sampled from prefix + suffix for fingerprint
)

// Bitmap tracks which lines in emails.txt have been terminally processed.
// Concurrency model: 400 workers set bits via atomic CAS; an auto-saver
// goroutine snapshots the bitmap to disk every N seconds using atomic loads.
type Bitmap struct {
	words       []uint64
	totalLines  int64
	fingerprint [32]byte
	emailsSize  int64
	done        int64 // atomic: count of set bits

	savePath string

	saveMu sync.Mutex // serialises on-disk writes; bits themselves are lock-free
}

// LoadOrCreate loads the checkpoint at ckptPath if it exists and matches the
// current emails file fingerprint; otherwise counts the file and returns a
// fresh zero bitmap. The returned bitmap remembers savePath for SaveAtomic.
func LoadOrCreate(ckptPath, emailsPath string) (*Bitmap, error) {
	fp, size, err := computeFingerprint(emailsPath)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}

	if b, err := tryLoad(ckptPath, fp, size); err == nil && b != nil {
		b.savePath = ckptPath
		log.Printf("[CHECKPOINT] Loaded: %d / %d lines done (%.2f%%)",
			b.Done(), b.totalLines, float64(b.Done())/float64(b.totalLines)*100)
		return b, nil
	} else if err != nil {
		log.Printf("[CHECKPOINT] Discarding %s: %v", ckptPath, err)
	}

	// Fresh bitmap: count lines in file.
	log.Printf("[CHECKPOINT] Counting lines in %s (one-time)...", emailsPath)
	t0 := time.Now()
	total, err := countLines(emailsPath)
	if err != nil {
		return nil, fmt.Errorf("count lines: %w", err)
	}
	log.Printf("[CHECKPOINT] Counted %d lines in %s", total, time.Since(t0).Round(time.Millisecond))

	words := make([]uint64, (total+63)/64)
	return &Bitmap{
		words:       words,
		totalLines:  total,
		fingerprint: fp,
		emailsSize:  size,
		savePath:    ckptPath,
	}, nil
}

func tryLoad(path string, wantFP [32]byte, wantSize int64) (*Bitmap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) < headerSize {
		return nil, fmt.Errorf("short header")
	}
	if string(data[0:4]) != magic {
		return nil, fmt.Errorf("bad magic")
	}
	if data[4] != version {
		return nil, fmt.Errorf("version %d unsupported", data[4])
	}
	total := int64(binary.LittleEndian.Uint64(data[8:16]))
	var fp [32]byte
	copy(fp[:], data[16:48])
	size := int64(binary.LittleEndian.Uint64(data[48:56]))
	savedDone := int64(binary.LittleEndian.Uint64(data[56:64]))

	if fp != wantFP || size != wantSize {
		return nil, fmt.Errorf("emails.txt fingerprint changed")
	}

	wordCount := (total + 63) / 64
	expectBytes := headerSize + wordCount*8
	if int64(len(data)) < expectBytes {
		return nil, fmt.Errorf("bitmap body truncated: have %d, want %d", len(data), expectBytes)
	}

	words := make([]uint64, wordCount)
	body := data[headerSize:]
	for i := int64(0); i < wordCount; i++ {
		words[i] = binary.LittleEndian.Uint64(body[i*8 : i*8+8])
	}

	b := &Bitmap{
		words:       words,
		totalLines:  total,
		fingerprint: fp,
		emailsSize:  size,
		done:        savedDone,
	}
	// Recompute done in case savedDone was stale.
	b.done = b.recountDone()
	return b, nil
}

func (b *Bitmap) recountDone() int64 {
	var c int64
	for _, w := range b.words {
		for w != 0 {
			c += int64(w & 1)
			w >>= 1
		}
	}
	return c
}

// Test reports whether the line at idx has been marked terminal.
func (b *Bitmap) Test(idx int64) bool {
	if idx < 0 || idx >= b.totalLines {
		return false
	}
	return atomic.LoadUint64(&b.words[idx>>6])&(uint64(1)<<(uint(idx)&63)) != 0
}

// Set marks line idx as terminally processed. Idempotent and thread-safe.
func (b *Bitmap) Set(idx int64) {
	if idx < 0 || idx >= b.totalLines {
		return
	}
	w := idx >> 6
	mask := uint64(1) << (uint(idx) & 63)
	for {
		old := atomic.LoadUint64(&b.words[w])
		if old&mask != 0 {
			return
		}
		if atomic.CompareAndSwapUint64(&b.words[w], old, old|mask) {
			atomic.AddInt64(&b.done, 1)
			return
		}
	}
}

// Done returns the current count of set bits.
func (b *Bitmap) Done() int64 { return atomic.LoadInt64(&b.done) }

// TotalLines returns the fixed line count the bitmap was sized for.
func (b *Bitmap) TotalLines() int64 { return b.totalLines }

// SaveAtomic snapshots the bitmap to disk via write-to-tmp + rename.
// Workers may write during the snapshot; we use atomic loads per word, so
// the saved image is a consistent-per-word (possibly slightly stale) view.
func (b *Bitmap) SaveAtomic() error {
	b.saveMu.Lock()
	defer b.saveMu.Unlock()

	tmp := b.savePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }

	header := make([]byte, headerSize)
	copy(header[0:4], magic)
	header[4] = version
	binary.LittleEndian.PutUint64(header[8:16], uint64(b.totalLines))
	copy(header[16:48], b.fingerprint[:])
	binary.LittleEndian.PutUint64(header[48:56], uint64(b.emailsSize))
	binary.LittleEndian.PutUint64(header[56:64], uint64(b.Done()))
	if _, err := f.Write(header); err != nil {
		f.Close()
		cleanup()
		return err
	}

	// Stream words in 1MB chunks so we don't allocate len(words)*8 all at once.
	const chunkWords = 1024 * 128 // 1 MB
	buf := make([]byte, chunkWords*8)
	for start := 0; start < len(b.words); start += chunkWords {
		end := start + chunkWords
		if end > len(b.words) {
			end = len(b.words)
		}
		for i := start; i < end; i++ {
			w := atomic.LoadUint64(&b.words[i])
			binary.LittleEndian.PutUint64(buf[(i-start)*8:], w)
		}
		if _, err := f.Write(buf[:(end-start)*8]); err != nil {
			f.Close()
			cleanup()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	return os.Rename(tmp, b.savePath)
}

// StartAutoSaver runs SaveAtomic every interval until the returned stop
// function is called, which also performs a final save synchronously.
func (b *Bitmap) StartAutoSaver(interval time.Duration) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := b.SaveAtomic(); err != nil {
					log.Printf("[CHECKPOINT] Save error: %v", err)
				}
			case <-stop:
				if err := b.SaveAtomic(); err != nil {
					log.Printf("[CHECKPOINT] Final save error: %v", err)
				}
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// computeFingerprint returns a SHA-256 over (size || first 64KB || last 64KB).
// Fast regardless of file size; catches regeneration, truncation, appends.
func computeFingerprint(path string) (fp [32]byte, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return fp, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fp, 0, err
	}
	size = st.Size()

	h := sha256.New()
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])

	sample := make([]byte, fpSampleSize)
	n, err := io.ReadFull(f, sample)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fp, 0, err
	}
	h.Write(sample[:n])

	if size > int64(fpSampleSize) {
		if _, err := f.Seek(-int64(fpSampleSize), io.SeekEnd); err != nil {
			return fp, 0, err
		}
		n, err := io.ReadFull(f, sample)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return fp, 0, err
		}
		h.Write(sample[:n])
	}

	copy(fp[:], h.Sum(nil))
	return fp, size, nil
}

// countLines counts '\n' bytes in the file, plus one if the file is non-empty
// and doesn't end with '\n'.
func countLines(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, 4*1024*1024)
	var count int64
	var lastByte byte
	var any bool
	for {
		n, err := f.Read(buf)
		if n > 0 {
			for i := 0; i < n; i++ {
				if buf[i] == '\n' {
					count++
				}
			}
			lastByte = buf[n-1]
			any = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if any && lastByte != '\n' {
		count++
	}
	return count, nil
}
