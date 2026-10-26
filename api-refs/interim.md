# interim — API reference

> Reference for the API surface manyup uses, derived from the instance's OpenAPI
> description (`https://interim.drifty.win/api/openapi.json`, API version 2.2.0,
> retrieved 2026-10-26) and checked against `internal/services/interim.go`.
> Human-readable documentation: <https://interim.drifty.win/api>.

| | |
|---|---|
| **Site** | `https://interim.drifty.win` |
| **API base** | `https://interim.drifty.win/api/v1` |
| **Endpoints used** | `POST /uploads`, `POST /uploads/{id}/parts`, `PUT /uploads/{id}/parts/{n}`, `POST /uploads/{id}/complete`, `DELETE /uploads/{id}`, `GET /keys/me`, `GET /status` |
| **Public URL** | `file.page_url`, e.g. `https://interim.drifty.win/f/{id}` |
| **Auth** | Bearer user hash `interim_...`; multipart parts, completion, and abort use the one-time `upload_token` |
| **Upload method** | One presigned `PUT` for small files, S3 multipart above the instance threshold |
| **Max file size** | 7,516,192,768 bytes (7 GiB) on this instance; the API schema allows up to the provider's 5 TiB object limit |
| **Retention** | 10 days, with a 200 GiB storage cap on this instance; `/status` is authoritative |

---

## Authentication

An upload may be authenticated with a user hash (`interim_...`). On an instance with
public uploads enabled, an intent can also be created anonymously and hashes can be
created with `POST /keys`; generated hashes are not approved until an admin approves
them. On an instance with public uploads disabled, `POST /keys` is unavailable, only
approved hashes may create intents, and anonymous intents are refused.

Send the hash as a bearer credential:

```
Authorization: Bearer interim_...
```

`X-API-Key: interim_...` is accepted by the server as an alternative, but the plugin
always uses the bearer form. Only creating an upload intent (and checking the hash)
uses this credential; part requests, completion, and abort authenticate with the
`upload_token` from the intent response.

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

An invalid or revoked hash is answered with `401 unauthorized`. With public uploads
disabled, an unapproved hash gets `403 upload_not_allowed` when it tries to create an
intent, and an anonymous intent gets `401 uploads_restricted`. User hashes are stored
as SHA-256 digests only; a lost hash cannot be recovered.

---

## Upload flow

Every intent answers with a `mode` of `single` or `multipart`. The server chooses the
mode from the declared size and its configured threshold; a client does not request it.
The intent reserves the declared size immediately.

### Step 1 — create an upload intent

`POST /api/v1/uploads` with a JSON body. `name` (≤ 180 characters) and `size`
(1 … provider limit) are required; `content_type` is optional.

```http
POST /api/v1/uploads HTTP/1.1
Authorization: Bearer interim_...
Content-Type: application/json

{"name":"sample.bin","size":12345,"content_type":"application/octet-stream"}
```

Errors on this call: `401 uploads_restricted`, `403 upload_not_allowed`,
`413 file_too_large`, `429 rate_limited`, `507 storage_full`.

### Single mode

```json
{
  "upload_id": "…",
  "mode": "single",
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

Send the raw file to `url` with exactly the returned `method`, URL and headers. Do not
send a multipart form. The signature covers the returned headers, so `Content-Type` and
`Content-Disposition` must match verbatim.

```http
PUT https://storage.example/…signature… HTTP/1.1
Content-Type: application/octet-stream
Content-Disposition: attachment; filename=sample.bin
Content-Length: 12345

<raw bytes>
```

`expires_at` is the expiry of the presigned PUT URL (2 hours); `intent_expires_at` is
the later deadline for completion. The declared size is the maximum the object may
become — the server verifies the real size afterwards.

### Multipart mode

Above its threshold the same create call answers with part metadata instead of a PUT
URL:

```json
{
  "upload_id": "…",
  "mode": "multipart",
  "part_size": 16777216,
  "part_count": 5,
  "parts_url": "https://interim.drifty.win/api/v1/uploads/…/parts",
  "status_url": "https://interim.drifty.win/api/v1/uploads/…",
  "concurrency": 4,
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

`part_size` is fixed by the server (at least 5 MiB, and raised as needed to stay under
10,000 parts); only the final part may be shorter. `part_count` covers all bytes of the
declared size. `concurrency` is a browser parallelism hint and is not a protocol
requirement.

#### 1. Request part URLs

Ask for 1–100 part URLs per request, with the upload token:

```http
POST /api/v1/uploads/UPLOAD_ID/parts HTTP/1.1
X-Upload-Token: UPLOAD_TOKEN
Content-Type: application/json

{"part_numbers":[1,2,3]}
```

```json
{
  "parts": [
    {"part_number": 1, "url": "https://storage.example/…signature…", "expires_at": "2026-10-25T09:30:00Z"},
    {"part_number": 2, "url": "https://storage.example/…signature…", "expires_at": "2026-10-25T09:30:00Z"},
    {"part_number": 3, "url": "https://storage.example/…signature…", "expires_at": "2026-10-25T09:30:00Z"}
  ]
}
```

#### 2. PUT each part

PUT the part's exact byte range to its URL. The part contract documents no required
request headers; the storage response carries the part's ETag in the **`ETag` response
header**.

```http
PUT https://storage.example/…signature… HTTP/1.1
Content-Length: 16777216

<part bytes>
```

#### 3. Report each part

Record the ETag and exact byte size with the API. Non-final parts must equal
`part_size`; the final part must equal the remaining bytes. `204 No Content` means the
part was recorded.

```http
PUT /api/v1/uploads/UPLOAD_ID/parts/1 HTTP/1.1
X-Upload-Token: UPLOAD_TOKEN
Content-Type: application/json

{"etag":"\"…\"","size":16777216}
```

`400 invalid_etag` / `invalid_part_size` mean the report does not match storage;
`413 part_size_mismatch` is returned at completion if the provider-side part does not
match.

#### 4. Resume if needed

`GET /api/v1/uploads/{id}` with `X-Upload-Token` returns the current state, including
`completed_parts`:

```json
{
  "upload_id": "…",
  "mode": "multipart",
  "state": "pending",
  "part_size": 16777216,
  "part_count": 5,
  "completed_parts": [1, 2, 3],
  "concurrency": 4,
  "expires_at": "2026-10-25T09:45:00Z",
  "complete_url": "https://interim.drifty.win/api/v1/uploads/…/complete",
  "abort_url": "https://interim.drifty.win/api/v1/uploads/…"
}
```

A client can request URLs for the missing parts and continue. The manyup plugin uses a
fresh intent for each upload attempt, so it does not resume.

### Completion

`POST` the intent's `complete_url` (or `/api/v1/uploads/{id}/complete`) with the
one-time token in `X-Upload-Token`. No bearer credential is needed.

```http
POST /api/v1/uploads/UPLOAD_ID/complete HTTP/1.1
X-Upload-Token: UPLOAD_TOKEN_FROM_INTENT
```

For a single PUT the server does a `HEAD` on the object. For multipart it first
verifies the reported part sizes and ETags, assembles the parts, and uses the staging
key as the live object. It then verifies the real size (greater than zero, no larger
than declared and the configured cap), consumes the quota reservation, and creates the
file record.

`201 Created` means committed; `200 OK` means the upload was already committed and the
existing file is returned (the endpoint is idempotent). Both carry:

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

Use `file.page_url` as the public download page.

- `409 upload_missing`: the single PUT has not become visible to the API yet; retry.
- `409 upload_parts_incomplete`: not all multipart parts are recorded yet; retry or
  report the missing parts first.
- `410 upload_expired`: the intent window ended and the temporary object was removed.

### Abort (optional)

`DELETE /api/v1/uploads/{id}` with `X-Upload-Token` aborts an in-progress multipart
upload, deletes any staging object, releases the reservation, and closes the intent. It
answers `204 No Content`; a committed upload answers `409`. Unclaimed intents are
removed by a janitor once the upload window expires.

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
| `GET /api/v1/status` | Public storage/upload status, including `public_uploads`, retention, per-file limit, and used/reserved bytes. |

Files are permanently deleted when their retention period ends — this instance reports
10 days and a 200 GiB cap through `GET /status`. When storage is full, uploads are
rejected with `507 storage_full`. Signed download URLs expire after 5 minutes and must
not be cached or redistributed. A newly committed multipart file can briefly answer
`403 Invalid or expired download token` before storage settles; requesting a fresh link
usually succeeds.

---

## Errors and limits

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_request` / `not_multipart` / `invalid_part` / `invalid_etag` / `invalid_part_size` | Malformed JSON, wrong upload mode, part number, ETag, or part size. |
| 401 | `unauthorized` / `uploads_restricted` | Missing or invalid user hash / upload token, or an anonymous upload while uploads are restricted. |
| 403 | `forbidden` / `upload_not_allowed` / `key_creation_disabled` | Invalid delete/upload token, unapproved hash, or public hash creation while uploads are restricted. |
| 404 | `not_found` | Unknown file, upload, or endpoint. |
| 409 | `upload_missing` / `upload_parts_incomplete` | The single PUT has not reached storage yet, or not all multipart parts are recorded. |
| 410 | `expired` / `upload_expired` | The file retention period or upload window has ended. |
| 413 | `file_too_large` / `part_size_mismatch` | Declared or actual object exceeds the cap, or a provider part does not match its declared size. |
| 429 | `rate_limited` | Too many requests; honour `Retry-After`. |
| 507 | `storage_full` | The instance's storage cap (200 GiB here) is reached. |

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
  `plugin.ErrCredentialRejected`. For a valid but unapproved hash it then consults
  `GET /status`: the hash is accepted when `public_uploads` is true, and rejected
  otherwise. `429` and `5xx` are inconclusive, so a valid hash is not discarded when the
  host is down.
- Pre-flight checks reject empty files only (the API requires at least one byte). The
  per-file limit and the multipart threshold are enforced by the server, so the plugin
  does not duplicate the configured cap in code.
- The intent name is `filepath.Base(filename)`, truncated to 180 characters with the
  extension preserved; the content type is guessed from the extension with
  `application/octet-stream` as the fallback.
- The plugin dispatches on `mode`. A response without `mode` is treated as `single`,
  matching the pre-multipart API.
- **Single:** the file is streamed straight from the uploader's progress reader to the
  presigned URL — never buffered — with `Content-Length` set to the declared size. The
  intent's `headers` are copied verbatim; if `Content-Type` is absent, the guessed type
  is used.
- **Multipart:** part URLs are requested 100 at a time. Parts are PUT and reported one
  by one, in ascending part-number order, because the uploader supplies one
  forward-only reader. Part PUTs send no extra headers, set `Content-Length`, and take
  the ETag from the response for the `{"etag","size"}` report. Before uploading, the
  plugin validates that `part_count` matches the declared size and `part_size`; a
  mismatch fails before any part request. The server's `concurrency` hint is ignored.
- Single PUTs and individual part PUTs use a copy of the shared HTTP client with its
  five-minute overall timeout removed, because a large upload can legitimately take
  longer; context cancellation (Ctrl+C) still aborts them. Control-plane calls keep the
  shared timeout.
- Completion is attempted up to 5 times on either `409 upload_missing` or
  `409 upload_parts_incomplete`, with exponential backoff starting at 500 ms. Every
  other non-2xx status is surfaced immediately through `statusError`, preserving the
  `status NNN` form the retry classifier matches.
- Any failure after an intent is created triggers a best-effort
  `DELETE abort_url` with the upload token, on its own 10-second timeout, so a failed
  attempt does not hold quota or leave a multipart staging object until the janitor
  runs.
- The result URL is `file.page_url`; the reported size is `file.size`, falling back to
  the declared size if the server omits it.
- The intent's `delete_token` is returned by the API but not persisted. These uploads
  are always made with the owning user hash, which can delete them through
  `DELETE /files/{id}`.
- `SupportsLargeUpload()` is `true`.

Optional live checks (each creates and deletes a real file):

```bash
# Single PUT
INTERIM_API_TOKEN=interim_... go test ./internal/services -tags live -run TestInterimLive -v

# Multipart: set the size above the instance's threshold (65 MiB when this was written)
INTERIM_API_TOKEN=interim_... INTERIM_MULTIPART_MB=65 \
  go test ./internal/services -tags live -run TestInterimMultipartLive -v
```

---

## Divergence from the implementation

- Anonymous uploads are part of the API but not supported by the plugin: it always sends
  the bearer hash and refuses to start without one.
- Only intent creation, multipart parts, completion, abort, and the credential check are
  implemented. File listing, metadata, download-link creation, and deletion are left to
  the API documentation and the interim web UI. `GET /status` is called only to decide
  whether an unapproved hash is usable.
- The plugin uses `Authorization: Bearer`, never the `X-API-Key` alternative.
- Multipart uploads are sequential: the plugin does not use `concurrency` and does not
  resume an intent across process restarts or uploader retries. A retry starts a fresh
  intent and a fresh multipart session.
- The `delete_token` from the intent is discarded rather than stored, because files
  uploaded through manyup are owned by the configured hash.
- A filename longer than 180 characters is truncated locally instead of being sent for a
  `400 invalid_request`.
- The plugin does not pre-check the server's per-file limit or multipart threshold; both
  can change per instance and the create-intent response is authoritative.
