# Mocha — API reference

> Captured from the live service on **2026-10-25** and verified end-to-end against
> `https://api.mocha.my` with a real API key. Every response body shown below was
> observed, not guessed, unless marked _from docs_.

| | |
|---|---|
| **Web** | <https://mocha.my> |
| **API base** | `https://api.mocha.my` |
| **Docs page** | <https://mocha.my/api-docs> (requires sign-in; content is client-rendered) |
| **Auth** | Required — Bearer API key |
| **Key format** | `mocha_` + 40 chars |
| **Key management** | <https://mocha.my/api-keys> |
| **Public link** | `https://mocha.my/share/{token}` |
| **Rate limit** | 60 requests / minute per API key |

Mocha is a paid-tier file host. A free account is capped at **150 GiB** storage and
**20 GiB** per file; the limits are per-account and must be read from
`GET /api/profile/summary`, never assumed.

---

## Authentication

Every request carries the API key as a Bearer token:

```http
Authorization: Bearer mocha_cMYZnmhKewxH4eRUS6i4qbYDxQU1cCuTCzZk9P19YUg
```

Requests without a key, or with a revoked key, return:

```json
{ "error": "Unauthorized" }
```
…with HTTP `401`.

### Managing keys

```bash
# list (prefixes only, never the secret)
curl "https://api.mocha.my/api/api-keys" -H "Authorization: Bearer $KEY"

# create — the full secret is shown exactly once
curl -X POST "https://api.mocha.my/api/api-keys" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"CLI Upload Key"}'

# revoke
curl -X DELETE "https://api.mocha.my/api/api-keys" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"id":"api_key_row_id"}'
```

`POST` response:

```json
{
  "key": "mocha_xxxxxxxxxxxxxxxxx",
  "keyMeta": { "id": "...", "name": "CLI Upload Key" },
  "warning": "This is the only time the full API key will be shown. Copy and store it now."
}
```

---

## Account limits

`GET /api/profile/summary` (verified 200):

```json
{
  "id": "6e19c84d-5abf-422f-bffd-7ea9c9b4de1f",
  "name": "drifty",
  "email": "mocha@drifty.win",
  "role": "user",
  "canUseApi": true,
  "canCreateHotlinks": false,
  "usedBytes": 286318441,
  "normalBytes": 286318441,
  "backupBytes": 0,
  "maxStorageBytes": 161061273600,
  "maxBackupStorageBytes": null,
  "maxFileSizeBytes": 21474836480,
  "fileExpiryDays": 30,
  "coldStorageAfterDays": null,
  "twoFactorEnabled": false,
  "storagePercent": 0.17776988508800665,
  "uploadsCount": 1,
  "sharesCount": 1,
  "memberSince": "2026-10-19T05:59:48.336Z",
  "lastSeenAt": "2026-10-25T03:13:34.512Z",
  "vipExpiresAt": null
}
```

Key fields:

| Field | Meaning |
|---|---|
| `canUseApi` | `false` disables the whole API for the account |
| `canCreateHotlinks` | `false` ⇒ `POST /api/permanent-links` returns **403** |
| `maxFileSizeBytes` | per-file ceiling; exceeding it yields **413** |
| `maxStorageBytes` / `usedBytes` | quota; exceeding it yields **413** |
| `fileExpiryDays` | uploaded files auto-expire after this many days |

There is also `GET /api/storage/available` (verified 200):

```json
{
  "usedBytes": 286318441,
  "availableBytes": 160774955159,
  "maxStorageBytes": 161061273600,
  "storagePercent": 0.17776988508800665,
  "maxBackupStorageBytes": null,
  "backupAvailable": false
}
```

---

## Upload paths

Mocha has **two** upload mechanisms, and which one you can use depends on the storage
node the account is routed to.

| Path | Endpoint | Notes |
|---|---|---|
| Direct (single request) | `POST /api/files` | Small files. **Not supported on every node** — returns `400`. Was returning `503` on every attempt during capture. |
| Multipart (presigned) | `/api/files/multipart/*` | The reliable path. Streams in parts straight to S3-compatible storage. |
| Multipart (relay) | `/api/files/multipart/part` | **Deprecated** — rejected by most nodes. Do not use. |

> **Verified 2026-10-25:** direct upload failed every time with `503
> {"error":"No storage nodes available"}` while multipart worked immediately against the
> same account. Treat direct upload as an optional fast path with a multipart fallback,
> or skip it entirely.

### Limits reported by `POST /api/files/multipart/init`

```json
{
  "partSizeBytes": 52428800,          // 50 MB — relay (deprecated) chunk size
  "directPartSizeBytes": 134217728,   // 128 MB — presigned chunk size
  "partUploadConcurrency": 16,        // parts to keep in flight
  "directPartUpload": true            // false ⇒ presigned flow unavailable
}
```

A requested part size is clamped to **5 MB – 1 GB** (100 MB when going through the
signed proxy) and rounded up so a file never exceeds 10,000 parts.

---

## Direct upload

`POST /api/files`

```bash
curl -X POST "https://api.mocha.my/api/files" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Length: 123456" \
  -H "x-file-name: song.flac" \
  -H "x-file-path: /test/" \
  -H "x-file-type: audio/flac" \
  -H "x-file-md5: d41d8cd98f00b204e9800998ecf8427e" \
  --data-binary "@./song.flac"
```

Headers:

| Header | Required | Notes |
|---|---|---|
| `Content-Length` | **yes** | must be present and positive, else `411` |
| `x-file-name` | yes | file name |
| `x-file-path` | no | destination folder, e.g. `/test/` |
| `x-file-type` | no | MIME type |
| `x-file-md5` | no | lowercase 32-char hex; malformed values are silently ignored |

Success is **`201`**:

```json
{
  "file": {
    "id": "...",
    "file_name": "...",
    "original_name": "song.flac"
  }
}
```

Failures:

| Status | Meaning |
|---|---|
| `400` | the account's node cannot take a single-request upload → **use multipart** |
| `411` | missing/zero `Content-Length` |
| `413` | per-file limit or quota exceeded |
| `503` | no storage nodes available (observed on this account) |

The destination node cannot be chosen.

---

## Multipart upload (presigned, recommended)

Four steps. All JSON bodies are sent with `Content-Type: application/json`.

### 1. Init

```bash
curl -X POST "https://api.mocha.my/api/files/multipart/init" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"originalName":"song.flac","size":177226028,"mimeType":"audio/flac","path":"/test/","directPartSizeBytes":1073741824}'
```

Verified response (`200`):

```json
{
  "uploadId": "YmRjMzE4MDEtNjViYy00NzY4LTlmNjctOTZlNGVmN2UxN2I4",
  "key": "EWlIv6oxJHMsZ5mqe_avT",
  "nodeId": "5b7b7792-a01d-4949-b55c-33eb36f748a3",
  "originalName": "manyup-mp-1792898836182.bin",
  "purpose": "normal",
  "partSizeBytes": 52428800,
  "directPartSizeBytes": 134217728,
  "partUploadConcurrency": 16,
  "directPartUpload": true
}
```

**Chunk by the `directPartSizeBytes` returned by the server, never by the value you
requested.** If `directPartUpload` is `false`, the presigned flow is unavailable for that
node and the deprecated relay must be used.

### 2. Sign the parts

```bash
curl -X POST "https://api.mocha.my/api/files/multipart/presigned" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"uploadId":"...","key":"...","nodeId":"...","originalName":"song.flac","path":"/test/","partNumbers":[1,2]}'
```

```json
{
  "urls": [
    { "partNumber": 1, "url": "https://mocha-sgp-1.s3.sgp.io.cloud.ovh.net/...&partNumber=1&uploadId=...&x-id=UploadPart" },
    { "partNumber": 2, "url": "https://..." }
  ]
}
```

The URLs are S3 presigned `UploadPart` URLs, valid ~6 hours (`X-Amz-Expires=21600`).
Calling this again for later part numbers re-signs without disturbing the session.

### 3. PUT each part

```bash
curl -X PUT "https://mocha-sgp-1.s3.../part1" \
  -H "Content-Type: application/octet-stream" \
  -H "Content-Length: 20971520" \
  --data-binary "@part1.bin"
```

Response is a bare `200` with the **`ETag` header**. Capture it verbatim — it already
includes the surrounding double quotes, and those quotes are what `complete` expects:

```
part 1: 200 etag="afe8f9b4c6a892badb3ab7d2287410d1"
```

### 4. Complete

```bash
curl -X POST "https://api.mocha.my/api/files/multipart/complete" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"uploadId":"...","key":"...","nodeId":"...","originalName":"song.flac","path":"/test/","size":177226028,"mimeType":"audio/flac","md5":"d41d8cd98f00b204e9800998ecf8427e","parts":[{"partNumber":1,"etag":"\"...\""}]}'
```

Success is **`201`** and returns the completed file record:

```json
{
  "file": {
    "id": "a6a92866-0c1d-4876-858a-cf765b24abc1",
    "file_name": "EWlIv6oxJHMsZ5mqe_avT",
    "original_name": "manyup-mp-1792898836182.bin",
    "size": 1060921,
    "mime_type": "application/octet-stream",
    "created_at": "2026-10-25T03:27:22.075Z",
    "expires_at": "2026-11-24T03:27:22.082Z",
    "status": "finalizing",
    "md5": null,
    "storage_key": "EWlIv6oxJHMsZ5mqe_avT/manyup-mp-1792898836182.bin",
    "purpose": "normal"
  }
}
```

Notes:

- `status` is `finalizing` immediately after upload — do not assume `ready`.
- `md5` is `null` until computed; `md5` is optional in the request.
- **`file.id` is the UUID you pass to the sharing endpoints. `file.file_name` is the
  opaque storage name used by the files API.**

### Abort

```bash
curl -X POST "https://api.mocha.my/api/files/multipart/abort" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"uploadId":"...","key":"...","nodeId":"...","originalName":"song.flac","path":"/test/","partNumbers":[1,2]}'
```

```json
{ "success": true }
```

### Deprecated relay (avoid)

`PUT /api/files/multipart/part?uploadId=...&key=...&nodeId=...&originalName=...&path=...&partNumber=N`
with a raw octet-stream body, returning `{"etag":"..."}`. Rejected by most nodes.

---

## Sharing a file

An uploaded file is **private**. The only way to get a public URL is to create a share.
Creating one is a separate call, and the response is the `token` that forms the public
link.

```bash
curl -X POST "https://api.mocha.my/api/shares" \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"fileId":"a6a92866-0c1d-4876-858a-cf765b24abc1","expiresInHours":null,"maxDownloads":10}'
```

Verified response (**`201`**):

```json
{
  "share": {
    "id": "03716f42-ba79-4bc1-bf63-3935c98a9a61",
    "token": "XdVnCDBadGR9gfIaK3dbmvKb72FOFe44",
    "file_id": "a6a92866-0c1d-4876-858a-cf765b24abc1",
    "file_name": "EWlIv6oxJHMsZ5mqe_avT",
    "original_name": "manyup-mp-1792898836182.bin",
    "file_size": 1060921,
    "mime_type": "application/octet-stream",
    "expires_at": null,
    "max_downloads": null,
    "download_count": 0,
    "is_active": true,
    "password_protected": false,
    "is_permanent_link": false
  }
}
```

Public URL: `https://mocha.my/share/{token}` — verified `200 text/html`.

Body fields:

| Field | Notes |
|---|---|
| `fileId` | the `file.id` UUID from the upload |
| `expiresInHours` | defaults to **24**; `null` = no expiry |
| `maxDownloads` | optional cap |
| `password` | optional, max 256 chars |
| `folderPath` | share a folder instead (`"/"` is rejected) |

### Permanent / direct links — unavailable by default

`POST /api/permanent-links` with `{"fileId":"..."}` returns **`403`** unless the account
has the hotlink permission (`canCreateHotlinks: true`):

```json
{ "error": "You don't have permission to create direct download links. Contact an admin to enable this." }
```

Consequently `https://mocha.my/dl/{token}` returned **`404`** for a share token on this
account. Always fall back to `/share/{token}`.

### Other share endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/shares` | list shares |
| `GET` | `/api/shares/{token}` | share metadata (name, size, MIME, active) |
| `PATCH` | `/api/shares/{token}` | `{"isActive":false}` to disable |
| `DELETE` | `/api/shares/{token}` | delete. **Deleting the file also removes its shares** — the token then 404s. |
| `POST` | `/api/shares/{token}/verify-password` | unlock a password-protected share |
| `POST` | `/api/shares/resolve` | token ↔ file id, for links you own |

---

## Files

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/files?path=/&includeSubfolders=1` | list (cursor pagination, `limit` default 100 / max 500) |
| `GET` | `/api/files/{fileName}` | `302` to a signed URL; accepts file id or storage name |
| `GET` | `/api/files/{fileId}/md5` | MD5 (404 with `{"md5":null}` until computed) |
| `GET` | `/api/files/{fileId}/presigned` | presigned download URL |
| `PATCH` | `/api/files/{fileName}` | rename via `{"originalName":"new.flac"}` |
| `DELETE` | `/api/files/{fileName}` | delete |
| `POST` | `/api/files/download-token` | short-lived one-time download URL |

Verified delete: `DELETE /api/files/EWlIv6oxJHMsZ5mqe_avT` → `200 {"success":true}`.
The list afterwards no longer contained the file.

### Checksums

`/share/{token}.md5` returns `{"error":"Checksum unavailable for this file"}` immediately
after upload, because the hash is computed asynchronously. When available it is served as
`text/plain` in `md5sum` format: `d41d8cd98f00b204e9800998ecf8427e  song.flac`.

---

## Errors and rate limiting

Errors are JSON: `{"error":"..."}`.

| Status | Meaning | Retry? |
|---|---|---|
| `400` | bad request / node can't direct-upload | no (switch to multipart) |
| `401` | missing or invalid API key | no |
| `403` | feature not enabled on the plan (e.g. hotlinks) | no |
| `404` | file/share not found | no |
| `411` | `Content-Length` missing or zero | no |
| `413` | per-file limit or storage quota exceeded | no |
| `415` | wrong content type (preview endpoint) | no |
| `429` | rate limited — honour `Retry-After` | **yes** |
| `500` | server error | **yes** |
| `503` | no storage nodes available | **yes** |

Every response carries `X-RateLimit-Limit`, `X-RateLimit-Remaining` and
`X-RateLimit-Reset` (seconds until reset). The observed limit is **60 req/min per key**.
A `429` includes `Retry-After`.

---

## manyup integration notes

`internal/services/mocha.go` implements this reference as follows:

1. `POST /api/files/multipart/init` with the file name, size and MIME type.
2. Chunk by the returned `directPartSizeBytes` (falling back to `partSizeBytes`).
3. `POST /api/files/multipart/presigned` in batches, then `PUT` each part and collect
   the `ETag` header verbatim.
4. `POST /api/files/multipart/complete` and read `file.id`.
5. `POST /api/shares` with `expiresInHours: null` and return
   `https://mocha.my/share/{token}`.
6. On any failure after init, best-effort `POST /api/files/multipart/abort`.

Credential: `API_TOKEN` (`manyup config set mocha API_TOKEN mocha_...`, or
`MANYUP_MOCHA_API_TOKEN`).

Optional service settings, set through the `options` map in `config.json`:

| Option | Default | Effect |
|---|---|---|
| `path` | `/` | destination folder, e.g. `/uploads/` |
| `shareExpiresInHours` | *(unset → null)* | share expiry; unset means the link never expires, so it lives as long as the file |
| `maxDownloads` | *(unset)* | cap the number of downloads on the share |
