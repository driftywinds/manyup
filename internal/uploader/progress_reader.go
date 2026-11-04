package uploader

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/driftywinds/manyup/internal/plugin"
)

var _ plugin.ProgressReporter = (*progressReader)(nil)

// progressReader wraps an io.Reader to track bytes read and periodically emit
// progress updates through the progress channel. It computes a smoothed upload
// speed using an exponential moving average.
type progressReader struct {
	r          io.Reader
	total      int64
	bytesRead  atomic.Int64
	uploaded   atomic.Int64
	reported   atomic.Bool
	service    string
	progressCh chan<- Progress
	done       chan struct{}
	stopped    chan struct{}
	closed     atomic.Bool

	// Speed tracking. prevBytes/prevTime/speed are only touched by the tick
	// goroutine; startTime is immutable.
	startTime time.Time
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
		startTime:  now,
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

// ReportUploaded implements plugin.ProgressReporter. Once a plugin reports
// uploaded bytes, the display uses them instead of bytes read, so a chunked
// upload does not show 100% while the network is still busy.
func (pr *progressReader) ReportUploaded(bytes int64) {
	pr.reported.Store(true)
	pr.uploaded.Store(bytes)
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
			// Once all bytes are read, suppress ticks until Close() sends the
			// final event — avoids a decaying speed display. A plugin that
			// reports uploaded bytes keeps ticking even after it has read the
			// whole file, because it may still be uploading.
			shown := pr.bytesRead.Load()
			if pr.reported.Load() {
				shown = pr.uploaded.Load()
			}
			if pr.total > 0 && shown >= pr.total {
				continue
			}
			pr.emitProgress()
		}
	}
}

func (pr *progressReader) emitProgress() {
	shown := pr.bytesRead.Load()
	if pr.reported.Load() {
		shown = pr.uploaded.Load()
	}
	now := time.Now()

	if pr.reported.Load() {
		// Chunked uploads complete in bursts. The 100 ms instantaneous delta
		// would spike on every chunk and then collapse to 0 B/s, so display the
		// overall average rate instead.
		if elapsed := now.Sub(pr.startTime).Seconds(); elapsed > 0 {
			pr.speed = float64(shown) / elapsed
		}
	} else {
		// Smoothed speed via exponential moving average (alpha=0.3).
		dt := now.Sub(pr.prevTime).Seconds()
		if dt > 0.05 {
			delta := shown - pr.prevBytes
			// Switching from read progress to uploaded progress can move the
			// counter backwards. Start the next window from the lower value
			// rather than reporting a negative speed.
			if delta < 0 {
				delta = 0
			}
			instant := float64(delta) / dt
			if pr.speed == 0 {
				pr.speed = instant
			} else {
				pr.speed = 0.3*instant + 0.7*pr.speed
			}
			pr.prevBytes = shown
			pr.prevTime = now
		}
	}

	var pct float64
	if pr.total > 0 {
		pct = float64(shown) / float64(pr.total) * 100
		if pct > 100 {
			pct = 100
		}
	}

	pr.progressCh <- Progress{
		Service: pr.service,
		State:   "uploading",
		Bytes:   shown,
		Total:   pr.total,
		Percent: pct,
		Speed:   pr.speed,
	}
}
