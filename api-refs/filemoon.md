# FileMoon — API reference

> Reference for the API surface manyup uses, derived from the official FileMoon
> API documentation (retrieved 2026-11-08) and checked against
> `internal/services/filemoon.go` and the live service.

| | |
|---|---|
| **Site** | `https://filemoon.org` |
| **API base** | `https://filemoon.org/api/v1` |
| **Endpoints used** | `GET /account`, `POST /files/upload` |
| **Auth** | `Authorization: Bearer <personal access token>` |
| **Upload method** | Single multipart POST ≤ 90 MB; 90 MB chunked multipart above that |
| **File limits** | 4 GB/file on free accounts; 15 GB/file on premium accounts |
| **Rate limit** | Per token and IP; maximum 60 requests/minute. `429` includes `Retry-After` |
| **Public URL** | `download_link` / `urls.page`, normally `https://filemoon.org/{id}/file` |

FileMoon is a video host with HLS processing, remote uploads, subtitles and file
management. manyup implements account verification and local-file upload only.

> **Do not commit the token.** Set it with
> `manyup config set filemoon <token>` or
> `MANYUP_FILEMOON_API_TOKEN`. The token is shown once when created under
> **User Panel → API & Remote Upload**.

---

## Authentication

Every API request uses:

```http
Authorization: Bearer YOUR_FILEMOON_TOKEN
Accept: application/json
```

The token permissions are split into `files:read`, `files:write` and
`remote:write`. manyup needs `files:read` for `/account` and `files:write` for
uploads.

### Account check

```http
GET https://filemoon.org/api/v1/account
```

```json
{
  "success": true,
  "data": {
    "id": "rk9zKY45m0lY",
    "username": "example_user",
    "email": "user@example.com",
    "status": true,
    "files": 20,
    "storage_bytes": 1846721934,
    "remote_jobs": {
      "pending": 0,
      "running": 0,
      "completed": 1,
      "failed": 0
    }
  }
}
```

The live account also returned a `premium` object (`active`, `status`,
`expires_at`) and no `remote_jobs` counters visible in the example. manyup only
checks that the request succeeds.

---

## Upload a local file

```http
POST https://filemoon.org/api/v1/files/upload
Content-Type: multipart/form-data
```

For files up to **90 MB**, manyup sends one request with:

| Field | Value |
|---|---|
| `file` | The file; original filename preserved |
| `visibility` | Optional `1` (public) or `0` (private); sent only if configured |

Example:

```bash
curl -sS -X POST "https://filemoon.org/api/v1/files/upload" \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Accept: application/json" \
  -F "file=@/path/to/video.mp4" \
  -F "visibility=1"
```

### Response shapes

The current documentation shows a modern envelope:

```json
{
  "success": true,
  "data": {
    "id": "0JgG7vRZzoYW",
    "urls": {
      "page": "https://filemoon.org/0JgG7vRZzoYW/file",
      "watch": "https://filemoon.org/0JgG7vRZzoYW/watch",
      "embed": "https://filemoon.org/0JgG7vRZzoYW/embed"
    }
  }
}
```

The live upload endpoint currently returns the legacy shape instead:

```json
{
  "type": "success",
  "shared_id": "Y2xm07RpGRO1",
  "download_link": "https://filemoon.org/Y2xm07RpGRO1/file",
  "status_url": "https://filemoon.org/upload/status/Y2xm07RpGRO1",
  "my_files_url": "https://filemoon.org/user/files",
  "storage_transfer": "pending",
  "file": {
    "name": "manyup-probe.mp4",
    "mime": "video/mp4",
    "extension": "mp4",
    "size": 9631,
    "category": "video",
    "is_video": true,
    "watch_url": "https://filemoon.org/Y2xm07RpGRO1/watch"
  }
}
```

manyup supports both and reports `download_link` or `data.urls.page`. If neither
is present it falls back to `https://filemoon.org/{id}/file`.

---

## Large files: chunked upload

For files above 90 MB, upload to the same `/files/upload` endpoint in 90 MB
chunks. Each request is `multipart/form-data` with the `file` part plus:

| Field | Meaning |
|---|---|
| `dzuuid` | Same unique ID for every chunk |
| `dzchunkindex` | 0-based chunk index |
| `dztotalchunkcount` | Total number of chunks |
| `dzchunksize` | Configured chunk size (90 MB) |
| `dztotalfilesize` | Whole file size |
| `dzchunkbyteoffset` | Offset of this chunk in the file |

There is no `dzlastchunk`; FileMoon detects completion when all chunks are
present. Every chunk must use the **same original filename** in the multipart
`file` part. Temporary local chunk filenames may differ, but the multipart
filename must not.

Intermediate chunk response:

```json
{
  "type": "success",
  "status": "success",
  "chunk": true,
  "finished": false,
  "message": "Chunk uploaded successfully."
}
```

The live service does not reliably include `finished` on intermediate
responses; manyup treats every chunk before the last as intermediate and only
requires a file ID on the final chunk. The final chunk returns the same legacy
upload response shown above with `shared_id` and `download_link`.

manyup streams each chunk directly from the file reader with an exact
`Content-Length`; it does not load the whole file into memory. A chunk failure is
not retried inside the plugin because the reader has advanced; the uploader’s
normal file-level retry starts over with a fresh reader.

---

## Errors

Success responses are HTTP 2xx. Errors use:

```json
{
  "success": false,
  "error": {
    "code": "TOKEN_ABILITY_DENIED",
    "message": "The token does not have the required permission."
  },
  "request_id": "2c7c4d7a-0e4d-4d92-bb53-e32f1e705dda"
}
```

| HTTP | Code | Meaning |
|---|---|---|
| `401` | `UNAUTHENTICATED` | Missing/invalid Bearer token |
| `403` | `TOKEN_DISABLED` / `TOKEN_EXPIRED` | Disabled, expired, IP-blocked or missing permission |
| `404` | `NOT_FOUND` | Owned file or remote job not found |
| `409` | `REMOTE_JOB_NOT_CANCELLABLE` | Remote job cannot be cancelled |
| `422` | `VALIDATION_ERROR` | Invalid fields or uploaded file |
| `429` | `RATE_LIMITED` | Rate limit; wait `Retry-After` |
| `503` | `API_DISABLED` | API temporarily disabled |

manyup routes HTTP failures through the shared `statusError` helper, preserving
`status NNN` so `429` and `5xx` are retried and `4xx` validation/auth failures
fail fast.

---

## manyup integration notes

`internal/services/filemoon.go`:

- **Service name.** `filemoon`, display name `FileMoon`. Registered in
  `internal/services/registry.go`.
- **Credential.** `API_TOKEN`, holding the personal access token. Required.
- **Credential check.** `GET /account` with the Bearer header. HTTP `401`/`403`
  reject; `429`/`5xx` are inconclusive.
- **Upload size split.** Up to 90 MB uses one multipart POST. Above 90 MB uses
  90 MB chunks with the documented Dropzone-style fields.
- **Visibility.** `manyup config set filemoon visibility 1` makes uploads
  public; `0` makes them private. If unset, the field is omitted and FileMoon’s
  default applies.
- **Progress.** Chunked uploads report cumulative uploaded bytes through
  `plugin.ProgressReporter`, so the CLI bar reflects completed chunks rather
  than read-ahead.
- **Response compatibility.** Supports both the current `{success,data}` envelope
  and the legacy `{type,shared_id,download_link}` response returned by the live
  service.
- **Timeout.** Uploads use a copy of the shared client with no whole-request
  timeout; context cancellation still aborts.
- **File limits.** Free accounts are documented at 4 GB/file and premium at
  15 GB/file. For files over 4 GB manyup checks `/account` first and refuses a
  free account before uploading, so it does not waste bandwidth filling up the
  free allowance. Files over 15 GB are rejected locally.
- **`SupportsLargeUpload()`** is `true`.

Config example:

```bash
manyup config set filemoon your_api_token
manyup config select filemoon
manyup upload video.mp4
```

The live service was verified with both a single 12-second MP4 upload and a
two-chunk upload. Both returned `shared_id`/`download_link`, and both probe files
were deleted through the API.

---

## Divergence from the implementation

- **Only account verification and local upload are implemented.** File listing,
  status, updates, deletes, subtitles, HLS cast URLs, and remote uploads are not
  exposed by manyup.
- **Legacy upload response.** The live upload endpoint returns
  `shared_id`/`download_link` rather than the documented `data.id`/`urls` shape.
  Both are parsed; the public link is the download/page URL.
- **No resumable chunk upload.** Chunks are uploaded once; a chunk network
  failure restarts the whole file at the uploader level.
- **No remote-upload support.** `remote:write` endpoints are not used.
- **Tier check for large files.** Files over 4 GB require a premium account;
  manyup checks the account tier before starting. Video validation is still
  left to FileMoon.
