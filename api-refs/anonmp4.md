# AnonMP4 — API reference

> Reference for the API surface manyup uses, derived from the official
> documentation (<https://anonmp4.to/api-docs>, retrieved 2026-11-04), the live
> website uploader, the API host (<https://anonmp4api.xyz/>), and
> `internal/services/anonmp4.go`.

| | |
|---|---|
| **Site** | `https://anonmp4.to` |
| **Control API** | `https://anonmp4.to` — `/get-upload-node`, `/get-video-node` |
| **Documented API base** | `https://anonmp4api.xyz` — `POST /upload`, `GET /info/{video_id}` |
| **Endpoints used** | `POST /get-upload-node`, chunk `POST` to the returned `upload_url`, `POST /get-video-node`; `POST /upload` as a fallback |
| **Public URL** | `watch_url`, e.g. `https://anonmp4.art/v/{video_id}` |
| **Auth** | None — anonymous uploads |
| **Upload method** | 2 MiB chunks, up to 5 in flight; documented single multipart POST as fallback |
| **Max file size** | 20 GB documented; the error example reports a 53,687,091,200-byte cap (50 GiB), so treat the server response as authoritative |
| **Retention** | Not documented; the upload response exposes no expiry metadata |

AnonMP4 is a video host, not a general file host. Uploads must be video files
(MP4, AVI, MKV, MOV, WebM, and other `video/*` formats). There is no account,
no API key, and no credential to configure.

> **Current endpoint status (2026-11-04):** the documented
> `POST https://anonmp4api.xyz/upload` route answers `503 Service Unavailable`
> from Apache, even for a no-file request. The AnonMP4 website does not use that
> route; it uses the node/chunk flow below. manyup therefore uses the live node
> flow first and keeps the documented single-POST endpoint as a fallback.

---

## Authentication

None. Every request can be made anonymously. The website's public uploader
sends a `client_token` field, but it is embedded in the public page, is not an
account credential, and grants no identity:

```
client_token = TVRBYTRMcTBwOTRMcTBJYzZNQzQ0TUhkN2M2T0R4N2M2TnEwTTFOYTR4N2E0
```

The plugin sends that token by default. It can be overridden with the
`client_token` config option:

```bash
manyup config set anonmp4 client_token <new-token>
```

---

## Live upload flow

This is the flow used by the AnonMP4 website and by manyup.

### Step 1 — allocate an upload node

```http
POST https://anonmp4.to/get-upload-node
Content-Type: multipart/form-data; boundary=…

client_token=TVRBYTRMcTBwOTRMcTBJYzZNQzQ0TUhkN2M2T0R4N2M2TnEwTTFOYTR4N2E0
filename=video.mp4
```

A successful response is JSON:

```json
{
  "status": true,
  "node_id": "MTY5ODt31OTAc6ODA4Na4E",
  "upload_url": "https://anonmp4-egde-751.juliet32cb69.workers.dev/upload/recever/…",
  "node_token": "z9HR0x7HM6Lb59g0Ln79i2Lm6J1L2Ri2P2Ji2PUV…"
}
```

`upload_url` is a short-lived Cloudflare Worker endpoint. `node_token` is used as
the `file_id` for every chunk and for the final metadata call. `node_id` is used
for the final metadata call.

### Step 2 — upload 2 MiB chunks

For each chunk, POST the raw bytes to `upload_url` with query parameters and
headers:

```http
POST https://anonmp4-egde-751.juliet32cb69.workers.dev/upload/recever/…?file_id=NODE_TOKEN&chunkIndex=1&totalChunks=3&filename=video.mp4
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="video.mp4"
Content-Range: bytes 0-2097151/4194427
X-Uploading-Mode: parallel
```

| Parameter | Meaning |
|---|---|
| `file_id` | The `node_token` returned by step 1 |
| `chunkIndex` | 1-based chunk number |
| `totalChunks` | `ceil(size / 2 MiB)` |
| `filename` | Original filename, URL-encoded |

`Content-Disposition` uses JavaScript `encodeURIComponent` on the filename.
`Content-Range` uses inclusive byte offsets and the total file size. The browser
uploads five chunks in parallel; manyup does the same.

A successful chunk response is `200` with a small plain-text range:

```
0-2097151/4194427
```

The website accepts any 2xx chunk response unless the body is JSON with
`status: "error"` or `success: false`. manyup follows the same rule.

### Step 3 — fetch the video info

```http
POST https://anonmp4.to/get-video-node
Content-Type: multipart/form-data; boundary=…

node_id=MTY5ODt31OTAc6ODA4Na4E
node_token=z9HR0x7HM6Lb59g0Ln79i2Lm6J1L2Ri2P2Ji2PUV…
client_token=TVRBYTRMcTBwOTRMcTBJYzZNQzQ0TUhkN2M2T0R4N2M2TnEwTTFOYTR4N2E0
filename=video.mp4
```

A successful response includes the public watch page:

```json
{
  "success": true,
  "video_id": "I3p7qkZVv3UOI8N",
  "title": "video.mp4",
  "thumbnail": "https://anonmp4.art/assets/img/processing.png",
  "watch_url": "https://anonmp4.art/v/I3p7qkZVv3UOI8N",
  "embed_url": "https://anonmp4.art/embed/I3p7qkZVv3UOI8N",
  "delete_url": "https://anonmp4.to/delete?id=I3p7qkZVv3UOI8N&hash=…",
  "upload_date": "2026-11-04 14:28:13"
}
```

The live service returns `anonmp4.art` watch URLs even though the documentation
shows `anonmp4.to`. Use `watch_url` as-is; do not construct it from the
documented host.

Processing is asynchronous. The `thumbnail` can still be the placeholder
`processing.png` when this response returns, and the video may not be playable
immediately.

---

## Documented single-POST fallback

The official API is one multipart POST:

```http
POST https://anonmp4api.xyz/upload
Content-Type: multipart/form-data; boundary=…

file=@video.mp4
```

It returns the documented envelope:

```json
{
  "success": true,
  "video_id": "NLEusIzEvLs3s3B",
  "title": "BigBuckBunny",
  "thumbnail": "https://cdn.anonmp4.to/thumbs/NLEusIzEvLs3s3B.webp",
  "watch_url": "https://anonmp4.to/v/NLEusIzEvLs3s3B",
  "embed_url": "https://anonmp4.to/embed/NLEusIzEvLs3s3B",
  "delete_url": "https://anonmp4.to/delete?id=NLEusIzEvLs3s3B&hash=uniqehash",
  "upload_date": "2026-01-08T10:30:00Z",
  "message": "File received and queued for processing"
}
```

The plugin sends a streaming multipart body with an exact `Content-Length` and a
single `file` field. As noted above, the current deployment answers this route
with Apache `503`, so it is only attempted if `/get-upload-node` fails before any
file bytes are sent.

### Documented errors

Non-2xx responses use one envelope:

```json
{
  "success": false,
  "error": {
    "code": "E_INVALID_SIZE",
    "message": "File size exceeds the 20GB limit",
    "details": {
      "max_size": 53687091200,
      "received_size": 107374182400
    }
  }
}
```

| Status | Meaning |
|---|---|
| 200 | Success |
| 400 | Bad request — invalid parameters |
| 404 | Not found — video does not exist |
| 413 | Payload too large — file exceeds the size limit |
| 415 | Unsupported media type — not a supported video format |
| 429 | Too many requests — rate limit exceeded |
| 500 | Internal server error |

| Code | Meaning |
|---|---|
| `E_NO_FILE` | No file was provided in the upload request |
| `E_INVALID_SIZE` | File exceeds the size limit |
| `E_INVALID_TYPE` | File is not a supported video format |
| `E_UPLOAD_FAILED` | Upload processing failed |
| `E_HANDLER_FAILED` | Upload handler failed |
| `E_VIDEO_NOT_FOUND` | Requested video does not exist |

A proxy or maintenance page can return an HTML body instead of the JSON
envelope. The plugin keeps the HTTP status in the error text so the uploader's
retry classifier recognises `429` and `5xx`.

---

## Video info (documented, unused)

The documented metadata endpoint is:

```http
GET https://anonmp4api.xyz/info/NLEusIzEvLs3s3B
```

It returns title, duration, watch/embed URLs, thumbnail, upload date, privacy,
and status. The plugin does not call it; the live flow's `get-video-node`
response already contains the watch URL. Like `/upload`, this endpoint currently
answers Apache `503`.

---

## manyup integration notes

`internal/services/anonmp4.go`:

- **Service name.** `anonmp4`, display name `AnonMP4`. It is registered in
  `internal/services/registry.go` next to the other built-ins.
- **Credentials.** None. `RequiredCredentials()` returns nothing and the plugin
  does not implement `plugin.CredentialVerifier`; `manyup config select anonmp4`
  is enough to enable it. The optional `client_token` option exists only because
  the website's public token may change.
- **Primary flow.** `POST /get-upload-node`, then 2 MiB chunks, then
  `POST /get-video-node`. The result URL is the `watch_url` from the final call.
- **Chunking.** `ceil(size / 2 MiB)` chunks; five chunks are read and uploaded
  per batch. The uploader supplies one forward-only reader, so the plugin reads
  each batch into a bounded buffer (up to 10 MiB) and uploads it concurrently.
  No file-sized buffer is allocated.
- **Chunk request.** Query `file_id=node_token`, `chunkIndex`, `totalChunks`,
  `filename`; headers `Content-Type: application/octet-stream`,
  `Content-Disposition` with a `encodeURIComponent` filename, `Content-Range:
  bytes start-end/size`, `X-Uploading-Mode: parallel`. The request also sends a
  browser-like `User-Agent`, `Origin`, and `Referer`.
- **Chunk retries.** A failed chunk is retried up to 3 times with 1 s/2 s
  backoff. Transport errors and `408`/`425`/`429`/`5xx` are retryable; other
  4xx statuses are not. If all attempts fail, the error keeps the chunk's
  `status NNN` so the outer uploader can retry the file.
- **Control calls.** `/get-upload-node` and `/get-video-node` post small
  multipart forms with `client_token`, `filename`, `node_id`, and `node_token`.
  A failure in node allocation falls back to the documented single-POST
  endpoint; a failure after chunks have been sent does not, to avoid uploading
  the file twice.
- **Documented fallback.** A streaming multipart body with a single `file` field
  and an exact `Content-Length`. It is only attempted when node allocation fails
  before any file bytes are sent.
- **File names.** `filepath.Base`, CR/LF replaced by `_`, and the
  `Content-Disposition` filename escaped the same way as JavaScript's
  `encodeURIComponent`.
- **Pre-flight checks.** Only empty files are rejected locally. The 20 GB cap
  and video-format check are enforced by the server.
- **Timeout.** Chunk and fallback uploads use a copy of the shared HTTP client
  with its overall timeout removed, because a 20 GB upload can outlive the
  five-minute default. Context cancellation (Ctrl+C) still aborts.
- **Result.** `UploadResult.Size` is the declared size; the live API does not
  return a byte count. The `video_id`, title, thumbnail, embed URL, delete URL,
  and upload date are discarded because `plugin.UploadResult` has no fields for
  them.
- **`SupportsLargeUpload()`** is `true`.

Manual check (replace the file with a real video):

```bash
manyup config select anonmp4
manyup upload video.mp4
```

A real 2 KiB CLI upload against the live service succeeded on 2026-11-04 and
returned `https://anonmp4.art/v/YoSXKQ2sHr2EXq9`.

The unit tests use local `httptest` servers for both flows: a node/chunk fake
that verifies every chunk's index, `Content-Range`, payload, query, and headers,
and a legacy single-POST fake for the fallback.

---

## Divergence from the implementation

- **The documented `POST /upload` route is currently unavailable.** The live
  website and the plugin use the node/chunk flow. The documented route remains
  implemented as a fallback in case it is restored.
- **The node/chunk API is undocumented.** It uses a public client token embedded
  in the website and can change without notice. If it does, the fallback and the
  `client_token` option provide a recovery path.
- **`GET /info/{video_id}` is not used.** manyup reports the watch URL from
  `get-video-node` and does not poll processing status, duration, or privacy.
- **`delete_url` is discarded.** `plugin.UploadResult` has no field for it, so an
  anonymous upload cannot be deleted through manyup after the command exits.
- **The 20 GB limit is not pre-checked.** The documented prose says 20 GB while
  the error example reports a 50 GiB `max_size`; the server response is
  authoritative.
- **Non-video files are not rejected locally.** A `.zip` upload fails at the
  server rather than in local validation.
- **No resumable uploads.** Chunks are retried individually, but a process
  restart starts a new node and a new upload.
- **Optional metadata is not sent.** The documentation mentions optional metadata
  and privacy settings but documents no fields, so the plugin sends only the file
  and the required upload metadata.
