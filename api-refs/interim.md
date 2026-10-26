# interim — API reference

> Reference for the API surface manyup uses, derived from the instance's OpenAPI
> description (`https://interim.drifty.win/api/openapi.json`, API version 2.1.0,
> retrieved 2026-10-26) and checked against `internal/services/interim.go`.
> Human-readable documentation: <https://interim.drifty.win/api>.

| | |
|---|---|
| **Site** | `https://interim.drifty.win` |
| **API base** | `https://interim.drifty.win/api/v1` |
| **Endpoints used** | `POST /uploads`, `POST /uploads/{id}/complete`, `DELETE /uploads/{id}`, `GET /keys/me` |
| **Public URL** | `file.page_url`, e.g. `https://interim.drifty.win/f/{id}` |
| **Auth** | Bearer user hash `interim_...` (admin-approved when uploads are restricted); completion and abort use the one-time `upload_token` |
| **Upload method** | Three-step direct-to-S3 presigned `PUT`, single request |
| **Max file size** | 5,368,709,120 bytes (5 GiB) — the single-PUT cap on this instance |
| **Retention** | Configured per instance — `/status` reports `retention_seconds` and the global storage cap |

---

## Authentication

Every upload is authenticated with a user hash issued by the admin in `/admin`.
Generated hashes are approved for uploads automatically. When the instance runs with
public uploads disabled, `POST /keys` is unavailable and only approved hashes may
create intents. Send the hash as a bearer credential:

```
Authorization: Bearer interim_...
```

`X-API-Key: interim_...` is accepted by the server as an alternative, but the plugin
always uses the bearer form. Only creating an upload intent (and checking the hash)
uses this credential; completion and abort authenticate with the `upload_token` from
the intent response.

`GET /keys/me` reports the approval flag and usage:

```json
{
  "id": "…",
  "key_prefix": "interim_…",
  "approved": true,
  "created_at": "2026-10-25T09:00:00Z",
  "files": 12,
  "bytes": 3456789,
  "last_used_at": "2026-10-26T06:56:53Z"
}
```

With public uploads disabled, an anonymous intent is refused with
`401 uploads_restricted`, and a valid but unapproved hash with
`403 upload_not_allowed`. User hashes are stored as SHA-256 digests only; a lost hash
cannot be recovered.

---

## Upload flow

### Step 1 — create an upload intent

`POST /api/v1/uploads` with a JSON body. `name` (≤ 180 characters) and `size`
(1 … 5,368,709,120 bytes) are required; `content_type` is optional.

```http
POST /api/v1/uploads HTTP/1.1
Authorization: Bearer interim_...
Content-Type: application/json

{"name":"sample.bin","size":12345,"content_type":"application/octet-stream"}
```

The intent reserves the declared size immediately and answers `201 Created`:

```json
{
  "upload_id": "…",
  "url": "https://storage.example/…signature…",
  "method": "PUT",
  "headers": {
    "Content-Type": "application/octet-stream",
    "Content-Disposition": "attachment; filename=sample.bin"
  },
  "expires_at": "2026-10-25T09:30:00Z",
  "intent_expires_at": "2026-10-25T09:45:00Z",
  "upload_token": "…",
  "complete_url": "https://interim.drifty.win/api/v1/uploads/…/complete",
  "abort_url": "https://interim.drifty.win/api/v1/uploads/…",
  "delete_token": "…",
  "delete_token_header": "X-Delete-Token",
  "warning": "…"
}
```

`expires_at` is the expiry of the presigned PUT URL (2 hours); `intent_expires_at`
is the later deadline for completion. The declared size is the maximum the object may
become — the server verifies the real size afterwards.

Errors on this call: `401 uploads_restricted`, `403 upload_not_allowed`,
`413 file_too_large`, `429 rate_limited`, `507 storage_full`.

### Step 2 — PUT the bytes

Send the raw file to the returned `url` with exactly the returned `method`, URL and
headers. Do not send a multipart form. The signature covers the returned headers, so
`Content-Type` and `Content-Disposition` must match the intent response verbatim.

```http
PUT https://storage.example/…signature… HTTP/1.1
Content-Type: application/octet-stream
Content-Disposition: attachment; filename=sample.bin
Content-Length: 12345

<raw bytes>
```

No file bytes pass through interim; this request goes straight to S3-compatible
storage. (CORS matters only for browser clients; a CLI sends the headers directly.)

### Step 3 — complete

`POST` the intent's `complete_url` (or `/api/v1/uploads/{id}/complete`) with the
one-time token in `X-Upload-Token`. No bearer credential is needed.

```http
POST /api/v1/uploads/UPLOAD_ID/complete HTTP/1.1
X-Upload-Token: UPLOAD_TOKEN_FROM_INTENT
```

The server does a `HEAD` on the object, accepts it only when its real size is greater
than zero and no larger than the declared size and the configured cap, consumes the
quota reservation, and creates the file record. `201 Created` means committed;
`200 OK` means the upload was already committed and the existing file is returned
(the endpoint is idempotent). Both carry:

```json
{
  "upload_id": "…",
  "file": {
    "id": "ieZsto9FY5k3Z5tt",
    "name": "sample.bin",
    "size": 12345,
    "content_type": "application/octet-stream",
    "created_at": "2026-10-25T09:30:00Z",
    "expires_at": "2026-11-04T09:30:00Z",
    "expires_in_seconds": 864000,
    "page_url": "https://interim.drifty.win/f/ieZsto9FY5k3Z5tt",
    "api_url": "https://interim.drifty.win/api/v1/files/ieZsto9FY5k3Z5tt",
    "owner": "user_hash"
  }
}
```

Use `file.page_url` as the public download page. `409 upload_missing` means the PUT
has not become visible to the API yet and should be retried; `410 upload_expired`
means the intent window ended and the temporary object was removed.

### Abort (optional)

`DELETE /api/v1/uploads/{id}` with `X-Upload-Token` deletes any object at the intent's
key, releases the reservation, and closes the intent. It answers `204 No Content`;
a committed upload answers `409`. Unclaimed intents are removed by a janitor once the
upload window expires.

---

## Files

These endpoints are public or owner-scoped and are not used by the plugin, but they
complete the lifecycle:

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/files/{id}` | Public metadata (the file record above). Never exposes the object key or delete token. |
| `POST /api/v1/files/{id}/download` | Public; returns `{"url", "filename", "expires_at"}` where `url` is valid for 5 minutes (signed S3 GET or worker-proxied token URL). |
| `DELETE /api/v1/files/{id}` | Owner bearer hash, or `X-Delete-Token: <delete_token>` for anonymous uploads. `204` on success. |
| `GET /api/v1/files?limit=50&offset=0` | The current hash's active files. |
| `GET /api/v1/status` | Public storage/upload status. |

Files are permanently deleted when their retention period ends — the instance reports it
via `GET /status`. Signed download URLs expire after 5 minutes and must not be cached
or redistributed.

---

## Errors and limits

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_request` | Malformed JSON, name too long, or size outside 1 … 5 GiB. |
| 401 | `unauthorized` / `uploads_restricted` | Missing or invalid user hash / upload token, or anonymous upload while uploads are restricted. |
| 403 | `forbidden` / `upload_not_allowed` / `key_creation_disabled` | Invalid delete/upload token, unapproved hash, or public hash creation while uploads are restricted. |
| 404 | `not_found` | Unknown file, upload, or endpoint. |
| 409 | `upload_missing` | PUT has not reached storage yet; retry completion. |
| 410 | `expired` / `upload_expired` | Retention period or upload window ended. |
| 413 | `file_too_large` | Declared or actual object exceeds the cap. |
| 429 | `rate_limited` | Too many requests; honour `Retry-After`. |
| 507 | `storage_full` | No storage capacity available. |

Every error uses one envelope:

```json
{
  "error": {
    "code": "storage_full",
    "message": "Storage maxed out — please try again later.",
    "request_id": "…"
  }
}
```

The server returns `X-Request-ID` on every response and sets
`Cache-Control: no-store` on API responses.

---

## manyup integration notes

`internal/services/interim.go`:

- Credential key: `API_TOKEN`, holding the `interim_...` user hash. The env fallback is
  `MANYUP_INTERIM_API_TOKEN`. `RequiredCredentials()` reports it as mandatory: the plugin
  always authenticates, so it also works when the instance disables anonymous uploads.
- `VerifyCredentials` calls `GET /keys/me`. `401`/`403` reject the hash via
  `plugin.ErrCredentialRejected`, and so does a `200` with `"approved": false`; `429`
  and `5xx` are treated as inconclusive so a valid hash is not discarded when the host
  is down.
- Pre-flight checks reject empty files (the API requires at least one byte) and files
  above the 5 GiB single-PUT cap before an intent is created. The server remains the
  authority on its own configured cap.
- The intent name is `filepath.Base(filename)`, truncated to 180 characters with the
  extension preserved; the content type is guessed from the extension with
  `application/octet-stream` as the fallback.
- The file is streamed straight from the uploader's progress reader to the presigned
  URL — never buffered — with `Content-Length` set to the declared size.
- The intent's `headers` are copied verbatim into the PUT. If `Content-Type` is absent,
  the guessed type is used.
- The PUT uses a copy of the shared HTTP client with its five-minute overall timeout
  removed, because a 5 GiB upload can legitimately take longer; context cancellation
  (Ctrl+C) still aborts it.
- Completion is attempted up to 5 times on `409 upload_missing` with exponential backoff
  starting at 500 ms. Every other non-2xx status is surfaced immediately through
  `statusError`, preserving the `status NNN` form the retry classifier matches.
- Any failure after an intent is created triggers a best-effort
  `DELETE abort_url` with the upload token, on its own 10-second timeout, so a failed
  attempt does not hold quota until the janitor runs.
- The result URL is `file.page_url`; the reported size is `file.size`, falling back to
  the declared size if the server omits it.
- The intent's `delete_token` is returned by the API but not persisted. These uploads
  are always made with the owning user hash, which can delete them through
  `DELETE /files/{id}`.
- `SupportsLargeUpload()` is `true`.

Optional live check (creates and deletes a real file, requires an approved hash):

```bash
INTERIM_API_TOKEN=interim_... go test ./internal/services -tags live -run TestInterimLive -v
```

---

## Divergence from the implementation

- Anonymous uploads are part of the API but not supported by the plugin: it always
  sends the bearer hash and refuses to start without one, which is what an instance with
  `public_uploads: false` requires.
- Only intent creation, completion, abort, and the credential check are implemented.
  Listing, metadata, download-link creation, and file deletion are left to the API
  documentation and the interim web UI.
- The plugin uses `Authorization: Bearer`, never the `X-API-Key` alternative.
- The `delete_token` from the intent is discarded rather than stored, because files
  uploaded through manyup are owned by the configured hash.
- A filename longer than 180 characters is truncated locally instead of being sent
  for a `400 invalid_request`.
