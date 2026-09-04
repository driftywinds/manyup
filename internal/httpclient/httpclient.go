// Package httpclient provides a shared, performance-tuned HTTP client for all
// upload services. Key optimizations over Go defaults:
//
//   - MaxIdleConnsPerHost raised from 2 → 20 for connection reuse
//   - Write/Read buffers raised from 4KB → 256KB to reduce syscalls
//   - Compression disabled (binary uploads don't compress and it wastes CPU)
//   - TCP keepalive and HTTP/2 forced on for maximum throughput
//   - Large copy buffer (256 KB) to minimize read/write syscalls on big files
package httpclient

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// CopyBufSize is the buffer size used for streaming file uploads.
// Go's io.Copy defaults to 32 KB; 256 KB cuts syscall count by 8×.
const CopyBufSize = 256 * 1024

// Copy streams src into dst using a large buffer for maximum throughput.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	buf := bufPool.Get().(*[]byte)
	n, err := io.CopyBuffer(dst, src, *buf)
	bufPool.Put(buf)
	return n, err
}

var bufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, CopyBufSize)
		return &b
	},
}

var (
	sharedClient *http.Client
	once         sync.Once
)

// Get returns a shared, performance-tuned HTTP client. It is safe for
// concurrent use by all upload services.
func Get() *http.Client {
	once.Do(func() {
		sharedClient = &http.Client{
			Timeout: 300 * time.Second, // 5 min default; services override per-request via context
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				MaxConnsPerHost:       20,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:  10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				WriteBufferSize:       256 * 1024, // 256 KB — reduces syscalls on large writes
				ReadBufferSize:        256 * 1024, // 256 KB — reduces syscalls on large reads
				DisableCompression:    true,        // binary uploads don't compress; saves CPU + memory
				DisableKeepAlives:     false,
			},
		}
	})
	return sharedClient
}
