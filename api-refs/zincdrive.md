# ZincDrive — API reference

> Reference for the vendor API surface used by manyup, checked against the vendor documentation.
> Official site: <https://zincdrive.com>.

| | |
|---|---|
| **API base** | `https://zincdrive.com/api/v1` |
| **Auth** | **Required** — `X-API-Key` header (or `api_key` query parameter) |
| **Upload method** | S3 presigned `PUT` + `/s3/confirm`; multipart available up to 10 GB |
| **Max file size** | 10 GB via multipart; 4 GB for a single-part `/s3/sign` upload |

---

## Authentication

Every API call carries the account API key — **not** a Bearer token:

```http
X-API-Key: {api_key}
```

The vendor also accepts the key as an `api_key` query parameter
(`/account?api_key=...`); the header is the recommended form. Rate limit is
**60 requests per minute** per key; exceeding it returns `429`.

---

## Upload flow

### Step 1 — sign

`POST /s3/sign` with `Content-Type: application/x-www-form-urlencoded`:

| Field | Value |
|---|---|
| `filename` | **required** — original filename including extension |
| `type` | optional MIME type, e.g. `video/mp4` |

```
filename=myfile.zip
```

```json
{
  "url": "https://s1.zincdrive.com/upload/bucket/...",
  "method": "PUT",
  "headers": { "content-type": "application/octet-stream" },
  "key": "uploads/users/abc123/myfile.zip"
}
```

There is no `status` envelope here. `url` and `key` are both required.
`headers` must be applied verbatim to the PUT — the signature covers them.
This single-part path is documented as the fastest method for files up to
**4 GB**.

### Step 2 — PUT the file

`PUT {url}` with the raw file body and the returned headers.

```http
PUT {presigned url}
content-type: application/octet-stream   # from sign response
```

If the sign response specifies no `Content-Type`, manyup sends
`application/octet-stream`.

### Step 3 — confirm

`POST /s3/confirm` as `application/x-www-form-urlencoded`. **Mandatory** — it
creates the file entry in the account after the S3 transfer:

| Field | Value |
|---|---|
| `key` | **required** — object key from the sign response |
| `filename` | **required** — original filename |
| `size` | **required** — total bytes |
| `folder` | optional folder id |
| `visibility` | `1` = public, `0` = private, `2` = unlisted (no default documented for this endpoint) |

```json
{
  "type": "success",
  "shared_id": "xyz789",
  "download_link": "https://zincdrive.com/file/xyz789"
}
```

`download_link` is the public URL, shaped
`https://zincdrive.com/file/{shared_id}`.

### Multipart (up to 10 GB)

Three steps, used by manyup for files above 4 GiB:

1. `POST /s3/multipart` — form-encoded `filename` (+ optional `type`). Returns
   `uploadId` and `key`.
2. `GET /s3/multipart/{id}/{part}` — returns a presigned URL for one part (the plugin
   accepts either a bare URL or `{"url": "..."}`). `PUT` the part bytes to it and keep the
   `ETag` response header.
3. `POST /s3/multipart/{id}/complete` — sends the collected ETags.

The vendor only states that completion takes "the list of ETags", so the exact body is
undocumented. The plugin sends both shapes (`parts` as an AWS-style
`[{PartNumber, ETag}]` array, and `etags` as a plain string array) and the server ignores
whichever it does not use.

`POST /s3/confirm` is still required afterwards, using the `key` returned by the initiate
call, to create the file record.

---

## Errors

| Status | Meaning |
|---|---|
| 401 | Missing or invalid API key |
| 403 | Subscription expired or feature not available |
| 404 | Resource not found |
| 422 | Validation error (check the `message` field) |
| 429 | Rate limit exceeded (60 req/min) |
| 500 | Server error |

Sign and confirm decode JSON on success only; any non-2xx response is rejected with
`zincdrive: status <code>: <body>`, which is the form the uploader's retry classifier
matches. The S3 `PUT` surfaces its own status and body the same way.

---

## manyup integration notes

`internal/services/zincdrive.go`:

- `RequiredCredentials()` returns `["API_TOKEN"]`; sent as `X-API-Key`.
- Dispatches on size: up to 4 GiB uses the single-part path, anything larger uses
  multipart. Both finish with the mandatory `/s3/confirm`.
- Single-part: `POST /s3/sign` with form-encoded `filename` (basename only) and `type`
  (MIME guessed from the extension), then `PUT` to the returned URL with every header the
  sign response supplied, falling back to `application/octet-stream`.
- Multipart: `POST /s3/multipart`, then per part `GET /s3/multipart/{id}/{n}` and a
  streamed `PUT` of that chunk (`Content-Length` set, 64 MiB parts), collecting ETags,
  then `POST /s3/multipart/{id}/complete`.
- `POST /s3/confirm` with `key`, `filename`, `size`, `visibility` (default `1`,
  sent explicitly) and optional `folder` taken from the `folder_id` option;
  `visibility` can also be overridden through `Options`.
- Returns `download_link` as the upload result URL.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (**required**; sent as `X-API-Key`).

---

## ⚠ Divergence from the implementation

- **Multipart response schema:** the vendor documents only that initiation "returns an
  uploadId and key" and that completion takes "the list of ETags". The plugin therefore
  reads several spellings (`uploadId`/`upload_id`/`id`, `key`/`objectKey`/`object_key`), a
  part URL as either a bare string or `{"url": ...}`, and sends completion both as
  `parts` and as `etags`. This is a best guess against an undocumented contract.
- **Visibility default:** the plugin always sends `visibility=1` when the `visibility`
  option is unset, but the vendor documents no default for `/s3/confirm`.
- **Auth subset:** the vendor's `api_key` query-parameter form is unsupported; only the
  `X-API-Key` header is sent.
- **Filename normalisation:** the plugin sends `filepath.Base(filename)`, so directory
  components are stripped before sign and confirm.
