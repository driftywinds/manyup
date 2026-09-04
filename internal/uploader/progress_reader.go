package uploader

import (
	"io"
	"sync/atomic"
	"time"
)

// progressReader wraps an io.Reader to track bytes read and periodically emit
// progress updates through the progress channel. It computes a smoothed upload
// speed using an exponential moving average.
type progressReader struct {
	r          io.Reader
	total      int64
	bytesRead  atomic.Int64
	service    string
	progressCh chan<- Progress
	done       chan struct{}
	stopped    chan struct{}
	closed     atomic.Bool

	// Speed tracking (only touched by the tick goroutine).
	prevBytes int64
	prevTime  time.Time
	speed     float64
}

func newProgressReader(r io.Reader, total int64, service string, progressCh chan<- Progress) *progressReader {
	now := time.Now()
	pr := &progressReader{
		r:          r,
		total:      total,
		service:    service,
		progressCh: progressCh,
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
		prevTime:   now,
	}
	// Emit initial progress event immediately so the display renders a bar.
	pr.progressCh <- Progress{
		Service: service,
		State:   "uploading",
		Total:   total,
	}
	go pr.tick()
	return pr
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.bytesRead.Add(int64(n))
	}
	return n, err
}

// Close stops the background ticker and waits for the final progress event
// to be emitted so the caller can safely send the terminal done/error event
// immediately after.
func (pr *progressReader) Close() {
	if pr.closed.CompareAndSwap(false, true) {
		close(pr.done)
		<-pr.stopped
	}
}

func (pr *progressReader) tick() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	defer close(pr.stopped)
	for {
		select {
		case <-pr.done:
			pr.emitProgress()
			return
		case <-ticker.C:
			// Once all bytes are read, suppress ticks until Close()
			// sends the final event — avoids decaying speed display.
			read := pr.bytesRead.Load()
			if pr.total > 0 && read >= pr.total {
				continue
			}
			pr.emitProgress()
		}
	}
}

func (pr *progressReader) emitProgress() {
	read := pr.bytesRead.Load()
	now := time.Now()

	// Smoothed speed via exponential moving average (alpha=0.3).
	dt := now.Sub(pr.prevTime).Seconds()
	if dt > 0.05 {
		instant := float64(read-pr.prevBytes) / dt
		if pr.speed == 0 {
			pr.speed = instant
		} else {
			pr.speed = 0.3*instant + 0.7*pr.speed
		}
		pr.prevBytes = read
		pr.prevTime = now
	}

	var pct float64
	if pr.total > 0 {
		pct = float64(read) / float64(pr.total) * 100
		if pct > 100 {
			pct = 100
		}
	}

	pr.progressCh <- Progress{
		Service: pr.service,
		State:   "uploading",
		Bytes:   read,
		Total:   pr.total,
		Percent: pct,
		Speed:   pr.speed,
	}
}
