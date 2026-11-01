# interim — API reference

> Reference for the API surface manyup uses, derived from the live OpenAPI
> description (`https://interim.drifty.win/api/openapi.json`, API version 2.2.0,
> retrieved 2026-11-01), the interim source repository
> (<https://github.com/driftywinds/interim>), and `internal/services/interim.go`.
> Human-readable documentation: <https://interim.drifty.win/api>.

| | |
|---|---|
| **Site** | `https://interim.drifty.win` (main instance; any instance can be self-hosted) |
| **API base** | `<origin>/api/v1` |
| **Endpoints used** | `POST /uploads`, `POST /uploads/{id}/parts`, `PUT /uploads/{id}/parts/{n}`, `POST /uploads/{id}/complete`, `DELETE /uploads/{id}`, `GET /keys/me`, `GET /status` |
| **Public URL** | `file.page_url`, e.g. `https://interim.drifty.win/f/{id}` |
| **Auth** | Optional bearer user hash `interim_...`; multipart parts, completion, and abort use the one-time `upload_token` |
| **Upload method** | One presigned `PUT` for small files, S3 multipart above the instance threshold |
| **Max file size** | Per instance: 7,516,192,768 bytes (7 GiB) on the main instance; 5 GB single-PUT cap on every instance |
| **Retention** | Per instance: 10 days on the main instance, sliding from the last download; `/status` is authoritative |

interim is open source and every instance is independently configured. The
origin, public-upload policy, per-file limit, retention, storage cap, and whether
multipart uploads are enabled can all differ between instances. Treat the
`GET /status` response and the create-intent response as authoritative rather
than assuming the main instance's values.

---

## Authentication

A user hash (`interim_...`) is optional. When an instance has public uploads
enabled, an upload intent can be created anonymously, and hashes can be created
with `POST /keys`; self-service hashes are not approved until an admin approves
them. When an instance has public uploads disabled, `POST /keys` is unavailable,
only approved hashes may create intents, and anonymous intents are refused with
`401 uploads_restricted`.

Send the hash as a bearer credential:

```
Authorization: Bearer interim_...
```

`X-API-Key: interim_...` is accepted by the server as an alternative, but the
plugin always uses the bearer form. Only creating an upload intent (and checking
the hash) uses this credential; part requests, completion, and abort authenticate
with the `upload_token` from the intent response.

`GET /keys/me` reports the approval flag, effective limits, and usage:

```json
{
  "id": "…",
  "key_prefix": "interim_…",
  "name": "cli",
  "approved": true,
  "created_at": "2026-10-25T09:00:00Z",
  "files": 12,
  "bytes": 3456789,
  "last_used_at": "2026-10-26T06:56:53Z",
  "max_file_size_bytes": 7516192768,
  "retention_seconds": 864000
}
```

`max_file_size_bytes` and `retention_seconds` are the hash's effective overrides;
`0` means unlimited for that hash.

An invalid or revoked hash is answered with `401 unauthorized`. With public
uploads disabled, an unapproved hash gets `403 upload_not_allowed` when it tries
to create an intent, and an anonymous intent gets `401 uploads_restricted`. User
hashes are stored as SHA-256 digests only; a lost hash cannot be recovered.

`POST /keys` creates a hash when public uploads are enabled:

```http
POST /api/v1/keys HTTP/1.1
Content-Type: application/json

{"name":"optional label"}
```

```json
{
  "id": "…",
  "user_hash": "interim_…",
  "key_prefix": "interim_…",
  "approved": false,
  "created_at": "2026-11-01T09:00:00Z",
  "max_file_size_bytes": 7516192768,
  "retention_seconds": 864000,
  "warning": "Save this user hash now. It is shown once and cannot be recovered."
}
```

The raw hash is returned once. manyup does not call this endpoint; an instance
hash is configured with `manyup config set interim <hash>` or supplied when the
instance is added.

---

## Upload flow

Every intent answers with a `mode` of `single` or `multipart`. The server chooses
the mode from the declared size and its configured threshold; a client does not
request it. The intent reserves the declared size immediately.

The declared size is exact, not an upper bound: completion requires the stored
object to have exactly the declared byte length. A short or long PUT fails with
`413 file_size_mismatch`.

### Step 1 — create an upload intent

`POST /api/v1/uploads` with a JSON body. `name` (≤ 180 characters) and `size`
(1 … provider limit) are required; `content_type` is optional (≤ 255
characters).

```http
POST /api/v1/uploads HTTP/1.1
Authorization: Bearer interim_...
Content-Type: application/json

{"name":"sample.bin","size":12345,"content_type":"application/octet-stream"}
```

The request body is decoded strictly: unknown JSON fields are rejected with
`400 invalid_request`.

Errors on this call: `400 invalid_request`/`empty_file`, `401 uploads_restricted`
or `unauthorized`, `403 upload_not_allowed`, `413 file_too_large`,
`429 rate_limited`, `507 storage_full`.

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

Send the raw file to `url` with exactly the returned `method`, URL and headers. Do
not send a multipart form. The signature covers the returned headers and the
declared `Content-Length`, so `Content-Type`, `Content-Disposition`, and the exact
byte count must match.

```http
PUT https://storage.example/…signature… HTTP/1.1
Content-Type: application/octet-stream
Content-Disposition: attachment; filename=sample.bin
Content-Length: 12345

<raw bytes>
```

`expires_at` is the expiry of the presigned PUT URL (5 minutes to 2 hours,
configured per instance); `intent_expires_at` is the later deadline for
completion. Files above the 5 GB single-PUT limit require multipart uploads.

### Multipart mode

Above its threshold the same create call answers with part metadata instead of a
PUT URL:

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

`part_size` is fixed by the server (5 MiB to 5 GiB, raised as needed to stay under
10,000 parts); only the final part may be shorter. `part_count` covers all bytes
of the declared size. `concurrency` is a browser parallelism hint and is not a
protocol requirement.

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

PUT the part's exact byte range to its URL. The part contract documents no
required request headers; the storage response carries the part's ETag in the
**`ETag` response header**. When the instance signs part lengths
(`MULTIPART_SIGN_PART_LENGTH=true`, the default), send a fixed-length body rather
than chunked transfer encoding.

```http
PUT https://storage.example/…signature… HTTP/1.1
Content-Length: 16777216

<part bytes>
```

#### 3. Report each part

Record the ETag and exact byte size with the API. Non-final parts must equal
`part_size`; the final part must equal the remaining bytes. `204 No Content` means
the part was recorded.

```http
PUT /api/v1/uploads/UPLOAD_ID/parts/1 HTTP/1.1
X-Upload-Token: UPLOAD_TOKEN
Content-Type: application/json

{"etag":"\"…\"","size":16777216}
```

`400 invalid_etag` / `invalid_part_size` mean the report does not match storage;
`413 part_size_mismatch` is returned at completion if the provider-side part does
not match.

#### 4. Resume if needed

`GET /api/v1/uploads/{id}` with `X-Upload-Token` returns the current state,
including `completed_parts`:

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
  "parts_url": "https://interim.drifty.win/api/v1/uploads/…/parts",
  "complete_url": "https://interim.drifty.win/api/v1/uploads/…/complete",
  "abort_url": "https://interim.drifty.win/api/v1/uploads/…"
}
```

A client can request URLs for the missing parts and continue. The manyup plugin
uses a fresh intent for each upload attempt, so it does not resume.

### Completion

`POST` the intent's `complete_url` (or `/api/v1/uploads/{id}/complete`) with the
one-time token in `X-Upload-Token`. No bearer credential is needed.

```http
POST /api/v1/uploads/UPLOAD_ID/complete HTTP/1.1
X-Upload-Token: UPLOAD_TOKEN_FROM_INTENT
```

For a single PUT the server does a `HEAD` on the object and copies it to an
unexposed live key. For multipart it first verifies the reported part sizes and
ETags, assembles the parts, and uses the staging key as the live object. It then
verifies the real size (greater than zero, exactly the declared size, and within
the configured per-hash cap), consumes the quota reservation, and creates the
file record.

`201 Created` means committed; `200 OK` means the upload was already committed and
the existing file is returned (the endpoint is idempotent). Both carry:

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
    "never_expires": false,
    "page_url": "https://interim.drifty.win/f/ieZsto9FY5k3Z5tt",
    "api_url": "https://interim.drifty.win/api/v1/files/ieZsto9FY5k3Z5tt",
    "owner": "user_hash"
  }
}
```

Use `file.page_url` as the public download page. `never_expires` is true when the
uploader's hash has unlimited retention; `expires_at` is then a far-future
sentinel.

- `409 upload_missing`: the single PUT has not become visible to the API yet; retry.
- `409 upload_parts_incomplete`: not all multipart parts are recorded yet; retry or
  report the missing parts first.
- `409 upload_busy`: another completion, abort, or expiry holds the intent; retry
  shortly.
- `409 upload_closed` / `410 upload_expired`: the intent window ended or the
  upload is no longer pending; the temporary object was removed.
- `429 completion_busy`: the instance's concurrent-completion limit was reached;
  retry completion shortly (honour `Retry-After`).
- `413 file_size_mismatch`: the stored object is not exactly the declared size.
- `502 storage_error`: a transient storage failure; completion is safe to retry.

### Abort (optional)

`DELETE /api/v1/uploads/{id}` with `X-Upload-Token` aborts an in-progress multipart
upload, deletes any staging object, releases the reservation, and closes the
intent. It answers `204 No Content`; a committed upload answers `409` (the code
can be `already_committed` or `upload_busy`). Unclaimed intents are removed by a
janitor once the upload window expires.

---

## Files

These endpoints are public or owner-scoped and are not used by the plugin, but they
complete the lifecycle:

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/files/{id}` | Public metadata (the file record above). Never exposes the object key or delete token. |
| `POST /api/v1/files/{id}/download` | Public; returns `{"url", "filename", "expires_at"}` where `url` is valid for 5 minutes by default (signed S3 GET or worker-proxied token URL). |
| `DELETE /api/v1/files/{id}` | Owner bearer hash, or `X-Delete-Token: <delete_token>` for anonymous uploads. `204` on success. |
| `GET /api/v1/files?limit=50&offset=0` | The current hash's active files. |
| `GET /api/v1/status` | Public storage/upload status, including `public_uploads`, `accepting_uploads`, retention, per-file limit, and used/reserved bytes. |

Files are permanently deleted when their retention period ends. The main instance
reports 10 days with sliding retention and no global storage cap through
`GET /status`; other instances can differ. When storage is full or the instance
is otherwise not accepting uploads, intents are rejected with `507 storage_full`.
Signed download URLs expire after 5 minutes and must not be cached or
redistributed. A newly committed multipart file can briefly answer `403 Invalid or
expired download token` before storage settles; requesting a fresh link usually
succeeds.

`GET /status` shape (2.2.0):

```json
{
  "accepting_uploads": true,
  "public_uploads": false,
  "storage": {
    "files": 0,
    "used_bytes": 0,
    "reserved_bytes": 0,
    "limit_bytes": 0,
    "available_bytes": 0,
    "used_percent": 0,
    "unlimited": true
  },
  "retention_seconds": 864000,
  "retention_human": "10 days",
  "retention_extend_on_download": true,
  "max_file_size_bytes": 7516192768,
  "download_url_ttl_seconds": 300,
  "server_time": "2026-11-01T08:29:58Z"
}
```

---

## Errors and limits

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_request` / `empty_file` / `not_multipart` / `invalid_part` / `invalid_etag` / `invalid_part_size` | Malformed JSON, empty file, wrong upload mode, part number, ETag, or part size. |
| 401 | `unauthorized` / `uploads_restricted` | Missing or invalid user hash / upload token, or an anonymous upload while uploads are restricted. |
| 403 | `forbidden` / `upload_not_allowed` / `key_creation_disabled` | Invalid delete/upload token, unapproved hash, or public hash creation while uploads are restricted. |
| 404 | `not_found` | Unknown file, upload, or endpoint. |
| 409 | `upload_missing` / `upload_parts_incomplete` / `upload_busy` / `upload_closed` / `upload_changed` / `already_committed` | Object not visible yet, parts missing, another operation in progress, intent closed, staging object changed, or abort after commit. |
| 410 | `expired` / `upload_expired` | The file retention period or upload window has ended. |
| 413 | `file_too_large` / `file_size_mismatch` / `part_size_mismatch` | Declared or actual object exceeds the cap, the stored object is not exactly the declared size, or a provider part does not match. |
| 429 | `rate_limited` / `completion_busy` | Too many requests, or too many concurrent completions; honour `Retry-After`. |
| 507 | `storage_full` | The instance's storage cap or pending-upload cap is reached. |

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

- **Instances.** The default instance is registered as service `interim`
  (`https://interim.drifty.win`). Every configured additional instance is
  registered as `interim@<label>` by `services.AllWithConfig(cfg)`, so it has its
  own selection flag, credentials, progress row, and upload result. The origin is
  stored in the service's `options.BASE_URL` (e.g.
  `services["interim@friends"].options.BASE_URL = "https://files.example.com"`);
  `BASE_URL` may also override the default instance's origin.
- **Labels.** Labels are 1–48 characters of lowercase letters, digits, `-`, `_`,
  and `.`, starting and ending with a letter or digit. The helper
  `SuggestInterimInstanceName` derives one from the hostname.
- **Adding instances.** `manyup interim add <url> [name] [hash]` validates the
  origin (no path, query, or fragment; `/api/v1` is accepted and stripped),
  optionally verifies the hash, stores `BASE_URL`, selects the new service, and
  saves. `manyup interim list` shows every instance; `manyup interim remove
  <name>` deletes one. The default instance cannot be removed, only deselected.
- **Credential key.** `API_TOKEN`, holding the `interim_...` user hash. It is
  optional: `RequiredCredentials()` returns nothing so an instance with public
  uploads enabled can be used anonymously. The env fallback
  `MANYUP_INTERIM_API_TOKEN` applies to the default instance; additional
  instances read their hash from the config entry.
- **Credential checks.** `VerifyCredentials` calls `GET /keys/me` when a hash is
  supplied. `401`/`403` reject the hash via `plugin.ErrCredentialRejected`. For a
  valid but unapproved hash it then consults `GET /status`: the hash is accepted
  when `public_uploads` is true, and rejected otherwise. Without a hash it calls
  `GET /status` directly and accepts anonymous use only when `public_uploads` is
  true. `accepting_uploads: false` is reported as an inconclusive failure rather
  than a credential rejection. `429` and `5xx` are inconclusive, so a valid hash is
  not discarded when the host is down.
- **Pre-flight checks.** Only empty files are rejected locally (the API requires
  at least one byte). The per-file limit, multipart threshold, storage cap, and
  instance availability are enforced by the server through the create-intent
  response, so the plugin does not duplicate instance policy in code.
- **Intent fields.** The intent name is `filepath.Base(filename)`, truncated to
  180 characters with the extension preserved; the content type is guessed from
  the extension with `application/octet-stream` as the fallback.
- **Mode dispatch.** A response without `mode` is treated as `single`, matching
  the pre-multipart API. Unknown modes fail before any bytes are sent.
- **Single PUT.** The file is streamed straight from the uploader's progress
  reader to the presigned URL — never buffered — with `Content-Length` set to the
  declared size, which the API now requires to be exact. The intent's `headers`
  are copied verbatim; if `Content-Type` is absent, the guessed type is used.
- **Multipart.** Part URLs are requested 100 at a time. Parts are PUT and reported
  one by one, in ascending part-number order, because the uploader supplies one
  forward-only reader. Part PUTs send no extra headers, set `Content-Length`, and
  take the ETag from the response for the `{"etag","size"}` report. Before
  uploading, the plugin validates that `part_count` matches the declared size and
  `part_size`; a mismatch fails before any part request. The server's
  `concurrency` hint is ignored. A failed part aborts the whole intent; the plugin
  does not resume a multipart session across attempts.
- **Streaming client.** Single PUTs and individual part PUTs use a copy of the
  shared HTTP client with its five-minute overall timeout removed, because a large
  upload can legitimately take longer; context cancellation (Ctrl+C) still aborts
  them. Control-plane calls keep the shared timeout.
- **Completion retries.** Completion is attempted up to 5 times on `409
  upload_missing`, `409 upload_parts_incomplete`, `409 upload_busy`, and any
  `429` (`completion_busy` or `rate_limited`), with exponential backoff starting
  at 500 ms. Retrying the same intent is safe because completion is idempotent;
  every other non-2xx status is surfaced immediately through `statusError`,
  preserving the `status NNN` form the retry classifier matches. The outer
  uploader may still retry a transient completion failure with a fresh intent.
- **Abort.** Any failure after an intent is created triggers a best-effort
  `DELETE abort_url` with the upload token, on its own 10-second timeout, so a
  failed attempt does not hold quota or leave a multipart staging object until the
  janitor runs.
- **Result.** The result URL is `file.page_url`; the reported size is
  `file.size`, falling back to the declared size if the server omits it.
- **Delete token.** The intent's `delete_token` is returned by the API but not
  persisted. A hash-owned upload can be deleted through `DELETE /files/{id}`;
  anonymous uploads would need the delete token, which the CLI currently
  discards.
- `SupportsLargeUpload()` is `true`.

Optional live checks (each creates and deletes a real file; `INTERIM_BASE_URL`
selects a self-hosted instance):

```bash
# Single PUT
INTERIM_API_TOKEN=interim_... go test ./internal/services -tags live -run TestInterimLive -v

# Multipart: set the size above the instance's threshold (65 MiB on the main instance)
INTERIM_API_TOKEN=interim_... INTERIM_MULTIPART_MB=65 \
  go test ./internal/services -tags live -run TestInterimMultipartLive -v
```

---

## Divergence from the implementation

- Anonymous uploads are supported now: with no `API_TOKEN` the plugin omits the
  bearer header and lets the instance accept or reject the intent.
- `POST /keys` is part of the API but the plugin does not create hashes. Configure
  an existing hash, or run anonymously on a public instance.
- Only intent creation, multipart parts, completion, abort, and the credential
  check are implemented. File listing, metadata, download-link creation, and
  deletion are left to the API documentation and the interim web UI.
  `GET /status` is called only by `VerifyCredentials`; uploads do not pre-flight
  it.
- The plugin uses `Authorization: Bearer`, never the `X-API-Key` alternative.
- Multipart uploads are sequential: the plugin does not use `concurrency` and
  does not resume an intent across process restarts or uploader retries. A retry
  starts a fresh intent and a fresh multipart session.
- The `delete_token` from the intent is discarded rather than stored, because
  files uploaded through manyup are normally owned by the configured hash. This
  means an anonymous upload cannot be deleted afterwards.
- A filename longer than 180 characters is truncated locally instead of being
  sent for a `400 invalid_request`.
- The plugin does not pre-check the server's per-file limit, multipart threshold,
  or storage cap; all of those can change per instance, and the create-intent
  response is authoritative.
- `GET /status` is consulted for credential checks, but its retention and storage
  fields are informational only; manyup does not display or enforce them.
