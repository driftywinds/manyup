# storage.to — API reference

> Verified against the live service on 2026-10-25. Both upload paths were run end to end
> (single-part and a 51 MB multipart upload); the bodies below are real responses.

| | |
|---|---|
| **Base URL** | `https://storage.to/api` |
| **Public URL** | `https://storage.to/{fileId}` |
| **Auth** | Optional — `Authorization: Bearer {token}` |
| **Upload method** | `POST /upload/init` → `PUT` to presigned URL → `POST /upload/confirm` |
| **Max file size** | 25 GB anonymous/free · 100 GB paid |
| **Anonymous expiry** | 3 days (up to 7 via `expiry_days`) |

All responses are JSON. A token is only needed for account features; anonymous uploads
work but are capped (50 files / 24 h, 100 GB / 24 h per visitor token, 500 GB / 24 h per
IP) and expire quickly.

---

## Authentication

```http
Authorization: Bearer {token}
```

`GET /user` answers `401 {"message":"Unauthenticated."}` without a valid token.

Anonymous clients can also send an ownership proof, either a visitor token or the
`owner_token` returned by upload calls:

```http
X-Visitor-Token: {random-string}
X-Owner-Token: {owner_v1_...}
```

manyup sends an account token when configured and does not use a visitor token, so
anonymous uploads are attributed by IP.

---

## Upload flow

### 1. Initiate

`POST /upload/init`

```json
{ "filename": "report.pdf", "content_type": "application/pdf", "size": 2202009 }
```

The response selects the upload shape. **Single** (files up to 50 MB):

```json
{
  "success": true,
  "type": "single",
  "upload_url": "https://7291236a….r2.cloudflarestorage.com/storage-to/c9ed…?X-Amz-Expires=1800&…",
  "headers": { "Content-Type": ["text/plain"] },
  "r2_key": "c9ed5c3e-f975-4d90-af7f-982385fe4da9"
}
```

**Multipart** (files above 50 MB):

```json
{
  "success": true,
  "type": "multipart",
  "upload_id": "cbf37564-6a8a-4109-a578-af60d9e6d8ce",
  "r2_key": "0817a613-93d7-42c9-940b-62a2a65b4674",
  "part_size": 33554432,
  "total_parts": 2,
  "initial_urls": { "1": "https://…partNumber=1&uploadId=…", "2": "https://…" },
  "owner_token": "owner_v1_…"
}
```

Notes:

- `part_size` is **32 MiB** in practice; chunk by the returned value, not by a constant.
- `headers` values are arrays, not strings. They are part of the signature and must be
  copied onto the PUT.
- `owner_token` is returned by multipart init and by confirm; the single init response
  does not include one.

### 2. Upload the bytes

`PUT {upload_url}` (single) — stream the whole file, applying the returned headers.

`PUT {initial_urls[n]}` (multipart) — one request per part, `Content-Length` set, and the
**`ETag` response header** kept for completion.

To sign parts that init did not cover:

`POST /upload/parts`

```json
{ "upload_id": "cbf37564-…", "part_numbers": [3, 4] }
```

```json
{ "success": true, "urls": { "3": "https://…", "4": "https://…" } }
```

> The published docs describe this response as `part_urls: [{partNumber, url}]`. The live
> service returns an **object keyed by part number**. manyup accepts both.

### 3. Complete and confirm

`POST /upload/complete-multipart` (multipart only)

```json
{
  "upload_id": "cbf37564-…",
  "parts": [
    { "partNumber": 1, "etag": "\"43c92bfc1255c5f1a8b7ff202369bebb\"" },
    { "partNumber": 2, "etag": "\"8c17c9caeff36b8ea0d37332df1fb572\"" }
  ]
}
```

```json
{ "success": true, "r2_key": "d06fd22f-1ab8-4110-9386-f309e44021bd" }
```

The ETag keeps the quotes it arrived with. Note that completion returns a **different**
`r2_key`; confirm still accepts the one from init (verified), and manyup uses that.

`POST /upload/confirm` — mandatory for both paths:

```json
{
  "filename": "mp-test.bin",
  "size": 53477376,
  "content_type": "application/octet-stream",
  "r2_key": "0817a613-93d7-42c9-940b-62a2a65b4674"
}
```

```json
{
  "success": true,
  "file": {
    "id": "3DH9K6Po3",
    "url": "https://storage.to/3DH9K6Po3",
    "filename": "mp-test.bin",
    "size": 53477376,
    "human_size": "51 MB",
    "expires_at": "2026-10-28T07:42:36+00:00"
  },
  "owner_token": "owner_v1_Q6qHdb…"
}
```

`file.url` is the shareable link.

### Abort

`POST /upload/abort` with `{"upload_id": "…"}` discards a partial multipart session.

> Share pages sit behind Cloudflare and return `403` ("Just a moment…") to non-browser
> clients, so a curl check of a finished link is not meaningful. Browsers open them fine.

---

## Errors

```json
{ "success": false, "error": "…" }
```

`GET /user` is the exception — it uses Laravel's `{"message": "Unauthenticated."}`.

| Status | Meaning | Retry? |
|---|---|---|
| `400` | bad request (e.g. collection size limit) | no |
| `401` | password required/incorrect, or bad token | no |
| `403` | not the owner of the resource | no |
| `404` | resource not found or expired | no |
| `422` | validation failed, or plan/quota restriction | no |
| `429` | rate limit or upload quota hit (sends `Retry-After`) | **yes** |
| `500` | server error | **yes** |

Rate limits are per IP: 60/min for init, confirm and abort; 120/min for part URLs; 500/min
for multipart completion.

---

## Other endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/file/reserve` | reserve an id + URL before the bytes exist |
| `GET` | `/file/{id}/status` | is the file still pending? |
| `DELETE` | `/file/{id}` | delete (owner) |
| `POST`/`DELETE` | `/file/{id}/password` | set/remove a password |
| `POST` | `/file/{id}/expiry` | change expiry (`days`, null = permanent on paid plans) |
| `POST` | `/file/{id}/max-downloads` | burn-after-N |
| `POST` | `/collection` | create a collection (`/c/{id}`) |
| `GET` | `/collection/{id}/status` | collection state |
| `POST` | `/sharex/upload` | one-shot multipart upload, 25 MB (20/day) |
| `GET` | `/user` | authenticated user |
| `GET` | `/bandwidth/status` | remaining upload quota |
| `GET` | `/health` | liveness (`{"status":"ok"}`) |

---

## manyup integration notes

`internal/services/storageto.go`, slug `storageto`:

- Optional `API_TOKEN` sent as a bearer token on every API call.
- `POST /upload/init` with `filename`, `content_type` (guessed from the extension) and
  `size`; the plugin then follows whatever shape came back (`type`), so it never guesses
  the multipart threshold itself.
- Single: streams the reader to `upload_url`, copying the init `headers` (string or array
  values; `Host` is applied via `req.Host`) and defaulting `Content-Type` to
  `application/octet-stream`.
- Multipart: uses `initial_urls`, requests any missing part numbers through
  `/upload/parts` in one call, streams each part with `Content-Length`, keeps the `ETag`,
  then posts `/upload/complete-multipart`. `owner_token` is sent as `X-Owner-Token`.
- On any multipart failure it calls `/upload/abort` (best effort, 10 s timeout).
- `POST /upload/confirm` returns `file.url`, which becomes the result URL.
- Optional `collectionId` option is passed as `collection_id` on confirm.
- Non-2xx responses go through `statusError`, keeping `status <code>` for the retry
  classifier.
- `VerifyCredentials` calls `GET /user`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (optional).
