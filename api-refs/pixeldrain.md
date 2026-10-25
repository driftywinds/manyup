# PixelDrain — API reference

> Derived from the vendor API documentation (<https://pixeldrain.com/api>, bundled copy:
> `pixeldrain.txt`). The integration notes describe `internal/services/pixeldrain.go`.

| | |
|---|---|
| **Site** | `https://pixeldrain.com` |
| **API base** | `https://pixeldrain.com/api` |
| **Public URL** | `https://pixeldrain.com/u/{fileId}` (viewer page); raw file: `https://pixeldrain.com/api/file/{fileId}` |
| **Auth** | Required — HTTP Basic; username ignored (empty), API key as password |
| **Upload method** | Raw `PUT` to `/api/file/{name}`; no multipart |
| **Max file size** | Plan-dependent (`subscription.file_size_limit` from `GET /api/user`) |

---

## Authentication

All methods that create, modify or delete a resource require an API key. Uploads are not
anonymous: `PUT /api/file/{name}` needs a key. Keys are created on the account's API keys
page.

The key goes in the **password** field of HTTP Basic authentication. The username is
ignored and can be empty or any value:

```http
Authorization: Basic base64(":" + api_key)
```

```bash
curl -T myfile.zip -u ":${PIXELDRAIN_API_KEY}" \
  https://pixeldrain.com/api/file/myfile.zip
```

A key is checked with `GET https://pixeldrain.com/api/user` using the same header; that
endpoint also returns the plan and current usage:

```json
{
  "id": "8e08a626-040e-417e-8713-a0f43737a235",
  "username": "some_user",
  "can_upload": true,
  "subscription": {
    "id": "prepaid",
    "file_size_limit": 107374182400,
    "file_expiry_days": 0,
    "storage_space": -1,
    "monthly_transfer_cap": -1
  },
  "storage_space_used": 1234567890,
  "file_count": 420
}
```

`can_upload` is `true` unless the account still needs a verified e-mail address (accounts
registered after 2026-09-01). Invalid or missing keys fail with `authentication_required`
/ `authentication_failed` (401); a key that is valid but not allowed for the resource
fails with `forbidden` (403).

---

## Upload

`PUT https://pixeldrain.com/api/file/{name}`

| Part | Location | Required | Notes |
|---|---|---|---|
| `name` | URL path | yes | max 255 characters |
| file bytes | request body | yes | max size is the plan's `file_size_limit` |

The body is the raw file; do **not** use multipart form encoding. Sending multipart to
this endpoint fails with `invalid_content_type` (400) — the multipart variant is
`POST /api/file`. The vendor recommends PUT over POST because multipart encoding can cause
performance problems.

```bash
curl -T myfile.zip -u ":${PIXELDRAIN_API_KEY}" \
  https://pixeldrain.com/api/file/myfile.zip
```

### Response

HTTP `201 Created`. The body is the full file info, the same structure as
`GET /api/file/{id}/info`:

```json
{
  "success": true,
  "id": "abc123",
  "name": "myfile.zip",
  "size": 12345678
}
```

`size` is in bytes. The remaining fields (`mime_type`, `hash_sha256`, `date_upload`,
`views`, `bandwidth_used`, `thumbnail_href`, …) are documented under
`GET /api/file/{id}/info`.

### Download link

- Viewer page: `https://pixeldrain.com/u/{id}` — the public link manyup returns.
- Direct file data: `GET https://pixeldrain.com/api/file/{id}`. Supports byte ranges;
  comma-separated IDs return a zip archive; `?download` forces an attachment header.
- Free downloads are subject to per-IP download, transfer and concurrency limits and may
  require a captcha (403).

### Limits

| Limit | Source | Error when exceeded |
|---|---|---|
| File size | `subscription.file_size_limit` (`GET /api/user`) | `file_too_large`, 413 |
| Account storage | `storage_space_used` vs plan | `user_out_of_space`, 400 |
| Files per account | plan | `too_many_files`, 400 |
| Downloads / transfer | per IP, free accounts | `download_limit_exceeded`, `transfer_limit_exceeded`, `max_concurrent_downloads`, 403 |

The limits are enforced server-side; a valid key does not guarantee the upload is accepted.

---

## Errors

Every JSON response carries `success`. A failure has HTTP status `400` or higher and adds:

| Field | Meaning |
|---|---|
| `value` | stable machine-readable error code — branch on this |
| `message` | human-readable text; may change between API versions |
| `extra` | optional object, e.g. `{"field": "name"}` |
| `errors` | array of error objects when `value` is `multiple_errors` |

Upload-relevant codes:

| `value` | HTTP | Cause |
|---|---|---|
| `authentication_required` | 401 | no API key sent |
| `authentication_failed` | 401 | key invalid, revoked or expired |
| `forbidden` | 403 | key not authorized for this resource |
| `invalid_content_type` | 400 | multipart body sent to the PUT endpoint |
| `file_too_large` | 413 | larger than the plan's file size limit |
| `no_file` | 422 | missing or empty file (POST endpoint) |
| `missing_field` | 400 | no file name (`extra.field`) |
| `string_out_of_range` | 422 | file name longer than 255 characters |
| `name_contains_illegal_character` | 422 | disallowed characters in the name (`extra.illegal_chars`) |
| `error_reading_input` | 400 | upload interrupted before completion |
| `too_many_files` | 400 | per-account file count limit |
| `user_out_of_space` | 400 | plan storage exhausted |
| `email_address_not_verified` | 403 | account has no verified e-mail address |
| `ip_banned` / `account_banned` | 403 | content-policy ban |
| `ip_rate_limit_reached` | 429 | IP rate limit for the action |
| `read_only_mode_enabled` | 503 | server is temporarily read-only |
| `writing` | 500 | server could not write the file |
| `internal` | 500 | server error |

```json
{
  "success": false,
  "value": "file_too_large",
  "message": "The file you tried to upload is too large"
}
```

`multiple_errors` returns HTTP 400, or 500 when one of the contained errors is a server
error, with the individual errors in the `errors` array. Download-side `403`/`451` codes
(captcha required, hotlink detected, takedown, …) are listed in the vendor docs under
`GET /file/{id}`; manyup does not consume them.

---

## manyup integration notes

Implementation: `internal/services/pixeldrain.go` (plugin name `pixeldrain`).

- **Request** — builds `PUT https://pixeldrain.com/api/file/{filepath.Base(filename)}`,
  streams the reader as the body, sets `Content-Length` when `size > 0`, and always sets
  `Content-Type: application/octet-stream`. The name is appended as-is (no escaping).
- **Auth** — sends `Authorization: Basic base64(":" + API_TOKEN)`. `RequiredCredentials()`
  returns `["API_TOKEN"]`, matching the vendor's rule that uploads require a key; the
  uploader now skips PixelDrain with a warning when no key is configured.
- **Parsing** — decodes `{success, id, name, size, value, message}`. `name` is decoded but
  unused; `UploadResult.Size` comes from the response's `size` (bytes) and
  `UploadResult.Filename` from the caller-supplied filename.
- **Success** — requires `success == true` and a non-empty `id`, otherwise an error. On
  `success == false` the error includes the HTTP status plus `value` and `message`. The
  returned URL is `https://pixeldrain.com/u/{id}`.
- **Credential check** — `VerifyCredentials` rejects an empty `API_TOKEN` itself, then calls
  `GET https://pixeldrain.com/api/user` with the same Basic header: `2xx` passes,
  `401`/`403` and other `4xx` are `ErrCredentialRejected`, `429`/`5xx` are inconclusive.
- **Large files** — `SupportsLargeUpload()` returns `true`; no client-side size check is
  made, so the plan limit is only enforced by the server.
- **Credential key** — `API_TOKEN` (env fallback `MANYUP_PIXELDRAIN_API_TOKEN`).

---

## ⚠ Divergence from the implementation

Maintainer notes — vendor documentation vs. `internal/services/pixeldrain.go`:

- **Upload auth** — *resolved*: the vendor requires an API key, and the plugin now declares
  `API_TOKEN` as a required credential instead of treating it as optional.
- **Status code** — the vendor returns `201 Created`; the plugin does not branch on the
  status, but every non-2xx error does embed `status <code>` (including body-parse
  failures), so transient `429`/`5xx` responses are still retried by the uploader.
- **Error detail** — the vendor can return `multiple_errors` plus per-field `extra`; the
  plugin reads only the top-level `value` and `message` and drops the rest.
- **File name in path** — the vendor treats `name` as a URL path segment (≤255 characters,
  illegal characters rejected with 422); the plugin appends the raw basename without
  percent-encoding or validation.
- **Response fields** — the vendor's PUT response is the full file info; the plugin consumes
  only `success`, `id` and `size`.
