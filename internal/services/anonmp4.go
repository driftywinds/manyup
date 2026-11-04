package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/driftywinds/manyup/internal/httpclient"
	"github.com/driftywinds/manyup/internal/plugin"
)

const (
	// anonMP4WebBase is the website that serves the upload-node control API.
	anonMP4WebBase = "https://anonmp4.to"

	// anonMP4UploadPath is the documented fallback endpoint.
	anonMP4UploadPath = "/upload"

	// anonMP4ResponseLimit caps how much of a control response is read.
	anonMP4ResponseLimit = 1 << 20

	// anonMP4ChunkSize is deliberately larger than the website's 2 MiB chunk.
	// The upload worker has high per-request latency, so 2 MiB chunks measured
	// at 0.3-0.9 MiB/s while 8 MiB chunks reached about 4 MiB/s on the same
	// connection. The worker accepts at least 16 MiB chunks.
	anonMP4ChunkSize int64 = 8 * 1024 * 1024

	// anonMP4ChunkParallel keeps five uploads in flight, matching the website's
	// browser concurrency.
	anonMP4ChunkParallel = 5
	anonMP4ChunkRetries  = 3

	// anonMP4ChunkTimeout bounds a single chunk request. The worker occasionally
	// leaves a request hanging; without a deadline one bad chunk can stall the
	// whole upload indefinitely.
	anonMP4ChunkTimeout = 90 * time.Second

	// anonMP4NodeCandidates is how many upload nodes are raced for the first
	// chunk of a large file. The node pool mixes healthy workers with ones that
	// answer 520/525 or move at a few hundred KB/s; the first successful first
	// chunk identifies a usable node.
	anonMP4NodeCandidates = 3

	// anonMP4NodeRaceTimeout bounds each candidate's first-chunk upload.
	anonMP4NodeRaceTimeout = 30 * time.Second

	// anonMP4ClientToken is the public client token embedded in the AnonMP4
	// website. It is not an account credential; it identifies the anonymous
	// upload client to the node allocator.
	anonMP4ClientToken = "TVRBYTRMcTBwOTRMcTBJYzZNQzQ0TUhkN2M2T0R4N2M2TnEwTTFOYTR4N2E0"

	anonMP4UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36"
)

var (
	// anonMP4WebBaseURL is a variable so tests can point the node API at a
	// local server.
	anonMP4WebBaseURL = anonMP4WebBase

	// anonMP4APIBase is the base of the documented single-POST API. The
	// documented /upload route is currently returning Apache 503, so it is
	// only used as a fallback when the upload-node API cannot be reached.
	anonMP4APIBase = "https://anonmp4api.xyz"

	// anonMP4ChunkRetryDelay is the base backoff for a failed chunk; tests
	// shrink it.
	anonMP4ChunkRetryDelay = time.Second

	// anonMP4NodeRaceMinSize is the file size at which the first-chunk node
	// race starts. It is a variable so tests can lower the threshold.
	anonMP4NodeRaceMinSize int64 = 64 << 20
)

// AnonMP4 uploads videos to anonmp4.to, an anonymous video host.
//
// The public documentation describes one multipart POST to
// https://anonmp4api.xyz/upload. That route is currently served by an Apache
// proxy that answers 503, so the plugin uses the same node/chunk upload flow as
// the AnonMP4 website:
//
//  1. POST /get-upload-node with a client token and filename.
//  2. POST each 8 MiB chunk to the returned upload_url with Content-Range and
//     query metadata.
//  3. POST /get-video-node to retrieve the final watch URL.
//
// If the node allocator is unavailable, the plugin falls back to the documented
// single-POST endpoint.
//
// See api-refs/anonmp4.md for the full contract.
type AnonMP4 struct{}

func RegisterAnonMP4(r *plugin.Registry) {
	r.Register(&AnonMP4{})
}

func (a *AnonMP4) Name() string        { return "anonmp4" }
func (a *AnonMP4) DisplayName() string { return "AnonMP4" }
func (a *AnonMP4) Description() string {
	return "AnonMP4 anonymous video hosting (20 GB max, chunked streaming upload)"
}

// RequiredCredentials is empty: the API is completely anonymous.
func (a *AnonMP4) RequiredCredentials() []string { return nil }

// SupportsLargeUpload reports true because chunks are read and uploaded one
// batch at a time, never buffering the whole file.
func (a *AnonMP4) SupportsLargeUpload() bool { return true }

// ── Response types ──────────────────────────────────────────────────

// anonMP4Error is the API's error envelope.
type anonMP4Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

// anonMP4UploadResponse is the success (and failure) envelope of the documented
// POST /upload endpoint.
type anonMP4UploadResponse struct {
	Success    bool          `json:"success"`
	VideoID    string        `json:"video_id"`
	Title      string        `json:"title"`
	Thumbnail  string        `json:"thumbnail"`
	WatchURL   string        `json:"watch_url"`
	EmbedURL   string        `json:"embed_url"`
	DeleteURL  string        `json:"delete_url"`
	UploadDate string        `json:"upload_date"`
	Message    string        `json:"message"`
	Error      *anonMP4Error `json:"error"`
}

// anonMP4Node is the response of POST /get-upload-node.
type anonMP4Node struct {
	Status    json.RawMessage `json:"status"`
	NodeID    string          `json:"node_id"`
	UploadURL string          `json:"upload_url"`
	NodeToken string          `json:"node_token"`
	Message   string          `json:"message"`
}

// anonMP4VideoInfo is the response of POST /get-video-node.
type anonMP4VideoInfo struct {
	Success   bool          `json:"success"`
	VideoID   string        `json:"video_id"`
	Title     string        `json:"title"`
	WatchURL  string        `json:"watch_url"`
	EmbedURL  string        `json:"embed_url"`
	DeleteURL string        `json:"delete_url"`
	Message   string        `json:"message"`
	Error     *anonMP4Error `json:"error"`
}

// ── Upload ──────────────────────────────────────────────────────────

func (a *AnonMP4) Upload(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
	creds plugin.Credentials,
	cfg plugin.Config,
) (*plugin.UploadResult, error) {
	if size <= 0 {
		return nil, fmt.Errorf("anonmp4: cannot upload an empty file")
	}

	clientToken := anonMP4ClientToken
	if cfg != nil {
		if configured := strings.TrimSpace(cfg["client_token"]); configured != "" {
			clientToken = configured
		}
	}
	uploadName := anonMP4FileName(filename)
	chunkSize, chunkParallel := anonMP4ChunkSettings(cfg)
	totalChunks := int((size + chunkSize - 1) / chunkSize)

	if totalChunks >= 4 && size >= anonMP4NodeRaceMinSize {
		return a.uploadWithNodeRace(ctx, filename, uploadName, reader, size, chunkSize, chunkParallel, clientToken)
	}

	node, nodeErr := a.getUploadNode(ctx, uploadName, clientToken)
	if nodeErr == nil {
		if err := a.uploadChunks(ctx, node, uploadName, reader, size, chunkSize, chunkParallel, 1, 0); err != nil {
			return nil, err
		}
		info, err := a.getVideoInfo(ctx, node, uploadName, clientToken)
		if err != nil {
			return nil, err
		}
		return &plugin.UploadResult{
			Service:  a.Name(),
			Filename: filename,
			Size:     size,
			URL:      info.WatchURL,
		}, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// The documented endpoint is dead on the current deployment, but keep it as
	// a fallback in case the node allocator is unavailable while /upload is
	// restored.
	result, legacyErr := a.uploadLegacy(ctx, filename, reader, size)
	if legacyErr != nil {
		return nil, fmt.Errorf("anonmp4: node API failed (%v); documented upload also failed: %w", nodeErr, legacyErr)
	}
	return result, nil
}

// uploadWithNodeRace is used for larger files. It reads the first chunk once,
// then uploads that same chunk to several freshly allocated nodes concurrently.
// The first successful node is kept and the rest are abandoned, so a slow or
// broken worker only costs the first chunk instead of the whole file.
func (a *AnonMP4) uploadWithNodeRace(
	ctx context.Context,
	filename, uploadName string,
	reader io.Reader,
	size, chunkSize int64,
	chunkParallel int,
	clientToken string,
) (*plugin.UploadResult, error) {
	totalChunks := int((size + chunkSize - 1) / chunkSize)

	first, err := anonMP4ReadChunk(reader, 1, chunkSize, size)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: reading first chunk: %w", err)
	}

	nodes, err := a.allocateNodes(ctx, uploadName, clientToken, anonMP4NodeCandidates)
	if err != nil {
		return nil, err
	}

	winner, err := a.raceFirstChunk(ctx, nodes, uploadName, totalChunks, size, first)
	if err != nil {
		return nil, err
	}
	if reporter, ok := plugin.ProgressReporterFromContext(ctx); ok {
		reporter.ReportUploaded(int64(len(first.data)))
	}

	if err := a.uploadChunks(ctx, winner, uploadName, reader, size, chunkSize,
		chunkParallel, 2, int64(len(first.data))); err != nil {
		return nil, err
	}

	info, err := a.getVideoInfo(ctx, winner, uploadName, clientToken)
	if err != nil {
		return nil, err
	}
	return &plugin.UploadResult{
		Service:  a.Name(),
		Filename: filename,
		Size:     size,
		URL:      info.WatchURL,
	}, nil
}

type anonMP4NodeResult struct {
	node *anonMP4Node
	err  error
}

func (a *AnonMP4) allocateNodes(ctx context.Context, filename, clientToken string, count int) ([]*anonMP4Node, error) {
	results := make(chan anonMP4NodeResult, count)
	for i := 0; i < count; i++ {
		go func() {
			node, err := a.getUploadNode(ctx, filename, clientToken)
			results <- anonMP4NodeResult{node: node, err: err}
		}()
	}

	nodes := make([]*anonMP4Node, 0, count)
	seen := make(map[string]bool, count)
	var firstErr error
	for i := 0; i < count; i++ {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
			}
			continue
		}
		if result.node == nil || seen[result.node.UploadURL] {
			continue
		}
		seen[result.node.UploadURL] = true
		nodes = append(nodes, result.node)
	}
	if len(nodes) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("anonmp4: no upload node available: %w", firstErr)
		}
		return nil, fmt.Errorf("anonmp4: no upload node available")
	}
	return nodes, nil
}

type anonMP4RaceResult struct {
	node *anonMP4Node
	err  error
}

func (a *AnonMP4) raceFirstChunk(
	ctx context.Context,
	nodes []*anonMP4Node,
	filename string,
	totalChunks int,
	size int64,
	chunk anonMP4Chunk,
) (*anonMP4Node, error) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan anonMP4RaceResult, len(nodes))
	for _, node := range nodes {
		go func(node *anonMP4Node) {
			chunkCtx, chunkCancel := context.WithTimeout(raceCtx, anonMP4NodeRaceTimeout)
			defer chunkCancel()
			err := a.uploadChunk(chunkCtx, node, filename, totalChunks, size, chunk)
			results <- anonMP4RaceResult{node: node, err: err}
		}(node)
	}

	var firstErr error
	for range nodes {
		result := <-results
		if result.err == nil {
			cancel()
			return result.node, nil
		}
		if firstErr == nil {
			firstErr = result.err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("all candidates failed")
	}
	return nil, fmt.Errorf("anonmp4: no upload node accepted the first chunk: %w", firstErr)
}

func anonMP4ReadChunk(reader io.Reader, index int, chunkSize, size int64) (anonMP4Chunk, error) {
	start := int64(index-1) * chunkSize
	length := chunkSize
	if remaining := size - start; remaining < length {
		length = remaining
	}
	if length <= 0 {
		return anonMP4Chunk{}, fmt.Errorf("invalid chunk %d length %d", index, length)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return anonMP4Chunk{}, err
	}
	return anonMP4Chunk{
		index: index,
		start: start,
		end:   start + length - 1,
		data:  data,
	}, nil
}

// ── Live node/chunk flow ───────────────────────────────────────────

// anonMP4ChunkSettings reads the optional tuning options. The defaults were
// chosen from measurements against the live worker; larger chunks amortize its
// per-request latency, and more workers help on high-bandwidth links.
func anonMP4ChunkSettings(cfg plugin.Config) (chunkSize int64, parallel int) {
	chunkSize = anonMP4ChunkSize
	parallel = anonMP4ChunkParallel
	if cfg == nil {
		return chunkSize, parallel
	}

	if raw := strings.TrimSpace(cfg["chunk_size_mb"]); raw != "" {
		if megabytes, err := strconv.Atoi(raw); err == nil && megabytes >= 1 && megabytes <= 32 {
			chunkSize = int64(megabytes) << 20
		}
	}
	if raw := strings.TrimSpace(cfg["chunk_parallel"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 16 {
			parallel = n
		}
	}
	return chunkSize, parallel
}

func (a *AnonMP4) getUploadNode(ctx context.Context, filename, clientToken string) (*anonMP4Node, error) {
	status, body, err := anonMP4PostForm(ctx, anonMP4WebBaseURL+"/get-upload-node", map[string]string{
		"client_token": clientToken,
		"filename":     filename,
	})
	if err != nil {
		return nil, fmt.Errorf("anonmp4: get-upload-node: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, statusError("anonmp4: get-upload-node", status, body)
	}

	var node anonMP4Node
	if err := json.Unmarshal(body, &node); err != nil {
		return nil, fmt.Errorf("anonmp4: parsing upload node: %w (body: %s)", err, describeBody(body))
	}
	if node.NodeID == "" || node.UploadURL == "" || node.NodeToken == "" {
		if node.Message != "" {
			return nil, fmt.Errorf("anonmp4: upload node unavailable: %s", node.Message)
		}
		return nil, fmt.Errorf("anonmp4: incomplete upload node response: %s", describeBody(body))
	}
	return &node, nil
}

func (a *AnonMP4) getVideoInfo(ctx context.Context, node *anonMP4Node, filename, clientToken string) (*anonMP4VideoInfo, error) {
	status, body, err := anonMP4PostForm(ctx, anonMP4WebBaseURL+"/get-video-node", map[string]string{
		"node_id":      node.NodeID,
		"node_token":   node.NodeToken,
		"client_token": clientToken,
		"filename":     filename,
	})
	if err != nil {
		return nil, fmt.Errorf("anonmp4: get-video-node: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, statusError("anonmp4: get-video-node", status, body)
	}

	var info anonMP4VideoInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("anonmp4: parsing video info: %w (body: %s)", err, describeBody(body))
	}
	if info.Error != nil {
		return nil, anonMP4PayloadError(info.Error)
	}
	if info.WatchURL == "" {
		if info.Message != "" {
			return nil, fmt.Errorf("anonmp4: video info has no watch_url: %s", info.Message)
		}
		return nil, fmt.Errorf("anonmp4: video info has no watch_url: %s", describeBody(body))
	}
	return &info, nil
}

// anonMP4Chunk is one slice of the file and its inclusive byte range.
type anonMP4Chunk struct {
	index int
	start int64
	end   int64
	data  []byte
}

// uploadChunks keeps the requested number of chunk uploads in flight
// continuously. The uploader supplies one forward-only reader, so a single
// producer reads the next chunk as soon as a worker is free. There is
// deliberately no batch barrier: waiting for the slowest chunk in a fixed group
// would idle the other workers and make the progress bar sit at 0 B/s between
// groups.
func (a *AnonMP4) uploadChunks(
	ctx context.Context,
	node *anonMP4Node,
	filename string,
	reader io.Reader,
	size, chunkSize int64,
	parallel, startIndex int,
	initialUploaded int64,
) error {
	if parallel < 1 {
		parallel = 1
	}
	if startIndex < 1 {
		startIndex = 1
	}
	totalChunks := int((size + chunkSize - 1) / chunkSize)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reporter, _ := plugin.ProgressReporterFromContext(ctx)
	var uploaded atomic.Int64
	uploaded.Store(initialUploaded)

	var (
		firstErrOnce sync.Once
		firstErr     error
	)
	recordErr := func(err error) {
		firstErrOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	// Unbuffered: the producer reads exactly one chunk ahead per free worker, so
	// memory stays bounded and disk reads track upload completions.
	jobs := make(chan anonMP4Chunk)

	var workers sync.WaitGroup
	for worker := 0; worker < parallel; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for chunk := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := a.uploadChunkWithRetry(ctx, node, filename, totalChunks, size, chunk); err != nil {
					recordErr(err)
					return
				}
				if reporter != nil {
					reporter.ReportUploaded(uploaded.Add(int64(len(chunk.data))))
				}
			}
		}()
	}

produce:
	for index := startIndex; index <= totalChunks; index++ {
		if ctx.Err() != nil {
			break
		}

		start := int64(index-1) * chunkSize
		length := chunkSize
		if remaining := size - start; remaining < length {
			length = remaining
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			recordErr(fmt.Errorf("anonmp4: reading chunk %d: %w", index, err))
			break
		}

		chunk := anonMP4Chunk{
			index: index,
			start: start,
			end:   start + length - 1,
			data:  data,
		}
		select {
		case jobs <- chunk:
		case <-ctx.Done():
			break produce
		}
	}

	close(jobs)
	workers.Wait()

	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (a *AnonMP4) uploadChunkWithRetry(
	ctx context.Context,
	node *anonMP4Node,
	filename string,
	totalChunks int,
	size int64,
	chunk anonMP4Chunk,
) error {
	var lastErr error
	for attempt := 1; attempt <= anonMP4ChunkRetries; attempt++ {
		err := a.uploadChunk(ctx, node, filename, totalChunks, size, chunk)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == anonMP4ChunkRetries || !anonMP4RetryableChunkError(ctx, err) {
			break
		}
		if !anonMP4Sleep(ctx, anonMP4ChunkRetryDelay*time.Duration(attempt)) {
			return ctx.Err()
		}
	}
	return lastErr
}

func (a *AnonMP4) uploadChunk(
	ctx context.Context,
	node *anonMP4Node,
	filename string,
	totalChunks int,
	size int64,
	chunk anonMP4Chunk,
) error {
	query := url.Values{}
	query.Set("file_id", node.NodeToken)
	query.Set("chunkIndex", strconv.Itoa(chunk.index))
	query.Set("totalChunks", strconv.Itoa(totalChunks))
	query.Set("filename", filename)

	endpoint := node.UploadURL
	if strings.Contains(endpoint, "?") {
		endpoint += "&" + query.Encode()
	} else {
		endpoint += "?" + query.Encode()
	}

	chunkCtx, cancel := context.WithTimeout(ctx, anonMP4ChunkTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(chunkCtx, http.MethodPost, endpoint, bytes.NewReader(chunk.data))
	if err != nil {
		return fmt.Errorf("anonmp4: creating chunk %d request: %w", chunk.index, err)
	}
	req.ContentLength = int64(len(chunk.data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Disposition", `attachment; filename="`+anonMP4EncodeURIComponent(filename)+`"`)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", chunk.start, chunk.end, size))
	req.Header.Set("X-Uploading-Mode", "parallel")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", anonMP4UserAgent)
	req.Header.Set("Origin", "https://anonmp4.to")
	req.Header.Set("Referer", "https://anonmp4.to/")

	resp, err := anonMP4StreamingClient().Do(req)
	if err != nil {
		return fmt.Errorf("anonmp4: chunk %d upload failed: %w", chunk.index, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &anonMP4ChunkHTTPError{status: resp.StatusCode, body: body}
	}
	if message := anonMP4ChunkRejection(body); message != "" {
		return fmt.Errorf("anonmp4: chunk %d rejected: %s", chunk.index, message)
	}
	return nil
}

// anonMP4ChunkHTTPError keeps the chunk's HTTP status available to both the
// per-chunk retry loop and the outer uploader's status-based retry classifier.
type anonMP4ChunkHTTPError struct {
	status int
	body   []byte
}

func (e *anonMP4ChunkHTTPError) Error() string {
	return fmt.Sprintf("anonmp4: chunk status %d: %s", e.status, describeBody(e.body))
}

func anonMP4RetryableChunkError(ctx context.Context, err error) bool {
	// A parent cancellation or deadline means the user (or the outer uploader)
	// stopped this attempt; retrying a chunk cannot help.
	if ctx.Err() != nil {
		return false
	}
	var httpErr *anonMP4ChunkHTTPError
	if errors.As(err, &httpErr) {
		// 400 is normally permanent, but the live worker's origin occasionally
		// answers 400 for a valid chunk (the website retries every chunk error
		// for the same reason). Retry it along with the usual transient codes.
		return httpErr.status == http.StatusBadRequest ||
			httpErr.status == http.StatusRequestTimeout ||
			httpErr.status == http.StatusTooEarly ||
			httpErr.status == http.StatusTooManyRequests ||
			httpErr.status >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// errors.Is(DeadlineExceeded) here is the per-chunk timeout, not the parent
	// context (checked above), so a retry is worthwhile.
	return true
}

// anonMP4ChunkRejection extracts an application-level rejection from a 2xx
// chunk response. The live worker answers with plain range text, which is not
// JSON and is therefore accepted.
func anonMP4ChunkRejection(body []byte) string {
	var payload struct {
		Status  json.RawMessage `json:"status"`
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Error   *anonMP4Error   `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if payload.Error != nil {
		return payload.Error.String()
	}
	if payload.Success != nil && !*payload.Success {
		if payload.Message != "" {
			return payload.Message
		}
		return "upload rejected"
	}
	switch strings.TrimSpace(string(payload.Status)) {
	case "false", "0", `"error"`, `"failed"`:
		if payload.Message != "" {
			return payload.Message
		}
		return "upload rejected"
	}
	return ""
}

func anonMP4Sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// anonMP4PostForm posts a small multipart form to the website API.
func anonMP4PostForm(ctx context.Context, endpoint string, fields map[string]string) (int, []byte, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return 0, nil, fmt.Errorf("writing form field %s: %w", key, err)
		}
	}
	if err := writer.Close(); err != nil {
		return 0, nil, fmt.Errorf("closing form: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return 0, nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", anonMP4UserAgent)
	req.Header.Set("Origin", "https://anonmp4.to")
	req.Header.Set("Referer", "https://anonmp4.to/")

	resp, err := httpclient.Get().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, anonMP4ResponseLimit))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// anonMP4EncodeURIComponent matches JavaScript's encodeURIComponent for the
// Content-Disposition filename used by the website uploader.
func anonMP4EncodeURIComponent(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	return strings.NewReplacer(
		"%21", "!",
		"%27", "'",
		"%28", "(",
		"%29", ")",
		"%2A", "*",
		"%7E", "~",
	).Replace(escaped)
}

// ── Documented single-POST fallback ────────────────────────────────

func (a *AnonMP4) uploadLegacy(
	ctx context.Context,
	filename string,
	reader io.Reader,
	size int64,
) (*plugin.UploadResult, error) {
	if size <= 0 {
		return nil, fmt.Errorf("anonmp4: cannot upload an empty file")
	}

	body, contentType, contentLength, err := anonMP4MultipartBody(filename, reader, size)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: preparing upload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anonMP4APIBase+anonMP4UploadPath, body)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: creating request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = contentLength

	resp, err := anonMP4StreamingClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("anonmp4: upload failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, anonMP4ResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("anonmp4: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, anonMP4ResponseError(resp.StatusCode, respBody)
	}

	var payload anonMP4UploadResponse
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil, fmt.Errorf("anonmp4: parsing response: %w (body: %s)", err, describeBody(respBody))
	}
	if payload.Error != nil {
		return nil, anonMP4PayloadError(payload.Error)
	}
	if !payload.Success {
		return nil, fmt.Errorf("anonmp4: upload failed: %s", describeBody(respBody))
	}
	if payload.WatchURL == "" {
		return nil, fmt.Errorf("anonmp4: response has no watch_url: %s", describeBody(respBody))
	}

	return &plugin.UploadResult{
		Service:  a.Name(),
		Filename: filename,
		Size:     size,
		URL:      payload.WatchURL,
	}, nil
}

// anonMP4MultipartBody builds a streaming multipart body with a known length.
func anonMP4MultipartBody(filename string, reader io.Reader, size int64) (io.Reader, string, int64, error) {
	boundary, err := anonMP4Boundary()
	if err != nil {
		return nil, "", 0, fmt.Errorf("generating boundary: %w", err)
	}

	name := anonMP4FileName(filename)
	contentType := anonMP4ContentType(name)

	prefix := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="file"; filename="` + anonMP4EscapeQuotes(name) + `"` + "\r\n" +
		"Content-Type: " + contentType + "\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"

	body := io.MultiReader(
		strings.NewReader(prefix),
		io.LimitReader(reader, size),
		strings.NewReader(suffix),
	)
	length := int64(len(prefix)) + size + int64(len(suffix))
	return body, "multipart/form-data; boundary=" + boundary, length, nil
}

func anonMP4Boundary() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "----manyup" + hex.EncodeToString(raw[:]), nil
}

// anonMP4StreamingClient returns the shared client without its overall timeout.
func anonMP4StreamingClient() *http.Client {
	client := *httpclient.Get()
	client.Timeout = 0
	return &client
}

// ── Errors ─────────────────────────────────────────────────────────

func anonMP4ResponseError(status int, body []byte) error {
	var payload struct {
		Error *anonMP4Error `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != nil {
		return fmt.Errorf("anonmp4: status %d: %s", status, payload.Error.String())
	}
	return statusError("anonmp4", status, body)
}

func anonMP4PayloadError(apiErr *anonMP4Error) error {
	return fmt.Errorf("anonmp4: %s", apiErr.String())
}

func (e *anonMP4Error) String() string {
	if e == nil {
		return "unknown error"
	}
	code := strings.TrimSpace(e.Code)
	message := strings.TrimSpace(e.Message)

	switch {
	case code != "" && message != "":
		text := code + ": " + message
		if len(e.Details) > 0 && string(e.Details) != "null" {
			text += " (" + describeBody(e.Details) + ")"
		}
		return text
	case code != "":
		return code
	case message != "":
		return message
	default:
		return "unknown error"
	}
}

// ── File naming and content type ───────────────────────────────────

var anonMP4VideoTypes = map[string]string{
	".3g2":  "video/3gpp2",
	".3gp":  "video/3gpp",
	".asf":  "video/x-ms-asf",
	".avi":  "video/x-msvideo",
	".divx": "video/divx",
	".f4v":  "video/x-f4v",
	".flv":  "video/x-flv",
	".m2ts": "video/mp2t",
	".m4v":  "video/mp4",
	".mkv":  "video/x-matroska",
	".mov":  "video/quicktime",
	".mp4":  "video/mp4",
	".mpeg": "video/mpeg",
	".mpg":  "video/mpeg",
	".mts":  "video/mp2t",
	".ogv":  "video/ogg",
	".ts":   "video/mp2t",
	".vob":  "video/mpeg",
	".webm": "video/webm",
	".wmv":  "video/x-ms-wmv",
}

func anonMP4ContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if contentType, ok := anonMP4VideoTypes[ext]; ok {
		return contentType
	}
	if contentType := mime.TypeByExtension(ext); strings.HasPrefix(strings.ToLower(contentType), "video/") {
		return contentType
	}
	return "application/octet-stream"
}

func anonMP4FileName(filename string) string {
	name := filepath.Base(filename)
	name = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "video"
	}
	return name
}

func anonMP4EscapeQuotes(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}
