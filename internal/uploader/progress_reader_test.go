package uploader

import (
	"strings"
	"testing"
)

func TestProgressReaderUsesReportedBytes(t *testing.T) {
	progressCh := make(chan Progress, 10)
	pr := newProgressReader(strings.NewReader("0123456789"), 100, "anonmp4", progressCh)
	t.Cleanup(pr.Close)

	// Simulate a chunked plugin that has read the whole file but only uploaded
	// 40 bytes so far. The first event is the initial 0% event from the
	// constructor.
	initial := <-progressCh
	if initial.Bytes != 0 || initial.Percent != 0 {
		t.Fatalf("initial progress = (%d, %.1f), want (0, 0)", initial.Bytes, initial.Percent)
	}

	pr.bytesRead.Store(100)
	pr.ReportUploaded(40)
	pr.emitProgress()

	got := <-progressCh
	if got.Bytes != 40 {
		t.Errorf("Bytes = %d, want 40 uploaded bytes, not read bytes", got.Bytes)
	}
	if got.Percent != 40 {
		t.Errorf("Percent = %.1f, want 40", got.Percent)
	}

	pr.ReportUploaded(100)
	pr.emitProgress()
	got = <-progressCh
	if got.Bytes != 100 || got.Percent != 100 {
		t.Errorf("final progress = (%d, %.1f), want (100, 100)", got.Bytes, got.Percent)
	}
}
