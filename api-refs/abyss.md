# Abyss.to — API reference

> Reference for the API surface manyup uses, derived from the official API
> document (`https://api.abyss.to`, retrieved 2026-11-05) and checked against
> `internal/services/abyss.go` and the live service.

| | |
|---|---|
| **Site** | `https://abyss.to` |
| **API base** | `https://api.abyss.to` |
| **Upload endpoint** | `POST https://up.abyss.to/{apiKey}` |
| **Endpoints used** | `POST /{apiKey}` on `up.abyss.to`; `GET /v1/about?key=…` for the credential check |
| **Public URL** | `https://abysscdn.com/?v={slug}` (the upload also returns `urlIframe`) |
| **Auth** | API key; embedded in the upload path and passed as `?key=` for `/v1/about` |
| **Upload method** | One streaming `multipart/form-data` POST, field `file` |
| **Max file size** | 10 GB on the checked account (`maxUploadSize` from `/v1/about`) |
| **Quota** | Per-account daily upload quota; `/v1/about` is authoritative |
| **Rate limit** | 1,200 requests per five minutes per user; exceeding it blocks API calls for five minutes with `429` |

Abyss.to is a video host, not a general file host. It accepts video files such as
MP4, MKV, and AVI, converts them asynchronously, and keeps them permanently.
Uploads are tied to an account API key.

> **Do not commit the API key.** Set it with
> `manyup config set abyss <key>`. The key is part of the upload URL, so only the
> HTTPS endpoint is used.

---

## Authentication

The upload API uses the API key as a path segment, not as an `Authorization`
header:

```http
POST https://up.abyss.to/{apiKey}
Content-Type: multipart/form-data; boundary=…
```

The documentation shows `http://up.abyss.to/{key}`; HTTPS works and is what the
plugin uses.

The management API accepts either a JWT from `POST /auth/login` or the API key
as a query parameter:

```http
GET https://api.abyss.to/v1/about?key={apiKey}
```

`GET /v1/about` returns quotas and limits:

```json
{
  "storageQuota": {
    "limit": 1125899906842624,
    "actual": 1099511627776,
    "usage": 0
  },
  "uploadQuota": {
    "dailyUploadQuota": 107374182400,
    "dailyUploadUsed": 0,
    "dailyUploadRemaining": 107374182400
  },
  "maxUploadSize": 10737418240,
  "perms": {
    "reconvert": false
  }
}
```

`maxUploadSize` is the per-file cap. `dailyUploadRemaining` is the account's
remaining upload quota for the day. manyup uses this endpoint only to verify a
key before saving it; it does not pre-check file size or quota.

---

## Upload

```http
POST https://up.abyss.to/{apiKey} HTTP/1.1
Content-Type: multipart/form-data; boundary=----manyup…
Accept: application/json

------manyup…
Content-Disposition: form-data; name="file"; filename="video.mkv"
Content-Type: video/x-matroska

<raw file bytes>
------manyup…--
```

The request body is a single file part named `file`. No other fields are
required. The API key is in the URL path.

The plugin builds the multipart body with a known `Content-Length`
(`prefix + file bytes + suffix`) and streams the file through `io.MultiReader`;
the file is never buffered. The part content type is selected from the video
extension (the same helper used by the AnonMP4 plugin), with
`application/octet-stream` as a fallback.

### Success response

`200 OK`:

```json
{
  "status": true,
  "msg": "Complete",
  "slug": "bOXsFvFr1",
  "urlIframe": "https://player.abyssplayer.com/bOXsFvFr1"
}
```

The documented sample only guarantees `{"slug":"file-id"}`, so `status`, `msg`,
and `urlIframe` are treated as optional extensions.

The public watch page is:

```
https://abysscdn.com/?v={slug}
```

The plugin reports that watch page. `urlIframe` is a separate player URL and is
not used. Conversion is asynchronous, so a newly uploaded video may be
processing for some time before it is playable.

### Error response

The upload endpoint does not use the management API's error envelope. A failed
upload can be:

```json
{"status": false, "msg": "error message"}
```

with HTTP `200`, or a normal HTTP error status (`400`, `401`, `403`, `413`,
`415`, `429`, `5xx`). The plugin preserves the HTTP status in the error text so
the uploader can retry `429` and `5xx`, while `4xx` validation failures fail fast.
A `200` with `status: false` is reported with the API `msg` and is not retried.

`GET /v1/about` errors use the management API's status codes: `401`/`403` for a
bad key, `429` for rate limiting, and `5xx` for an unavailable service. manyup
treats `401`/`403` as a rejected credential and `429`/`5xx` as inconclusive.

---

## Other endpoints (not used by manyup)

The management API also provides:

- `GET /v1/resources` and `GET /v1/files/{id}` — list and inspect files
- `PUT`/`PATCH`/`DELETE /v1/files/{id}` — rename, move, delete
- `POST`/`GET`/`PUT`/`PATCH`/`DELETE /v1/folders…` — folder management
- `POST /v1/remote/drive/{fileId}` — import a Google Drive file
- `GET`/`PUT`/`DELETE /v1/subtitles…` — subtitle management

These require a user JWT for write operations on the checked account; the API key
works for the read-only quota and resource calls. manyup only implements upload
and credential verification.

---

## manyup integration notes

`internal/services/abyss.go`:

- **Service name.** `abyss`, display name `Abyss.to`. Registered in
  `internal/services/registry.go`.
- **Credential key.** `API_TOKEN`, holding the account API key. It is required;
  the uploader skips the service when it is missing.
- **Credential check.** `GET https://api.abyss.to/v1/about?key=…`. `200` is
  accepted, `401`/`403` rejects the key via `plugin.ErrCredentialRejected`, and
  `429`/`5xx` are inconclusive. The key is query-escaped.
- **Upload.** Streaming multipart with one `file` part and an exact
  `Content-Length` to `https://up.abyss.to/{url.PathEscape(apiKey)}`. HTTPS is
  used even though the documentation shows HTTP.
- **Timeout.** The plugin uses a copy of the shared HTTP client with its overall
  timeout removed, because a 10 GB upload can exceed five minutes. Context
  cancellation still aborts.
- **Response.** `slug` is required. The result URL is
  `https://abysscdn.com/?v=` plus the query-escaped slug. The declared file size
  is reported because the upload response has no size field.
- **Errors.** Non-2xx responses go through `statusError`, preserving
  `status NNN` for the uploader's retry classifier. A `200` with `status:false`
  becomes `abyss: <msg>`; a missing slug becomes an explicit error.
- **Pre-flight checks.** Only empty files are rejected locally. `maxUploadSize`,
  daily quota, and video-format checks are enforced by the server.
- **`SupportsLargeUpload()`** is `true`.

Config example:

```bash
manyup config set abyss <your-api-key>
manyup config select abyss
manyup upload video.mkv
```

The live upload endpoint was verified with a multipart POST and returned
`status:true`, a slug, and an `urlIframe`; the watch URL derived from that slug
resolved to the uploaded file's title.

---

## Divergence from the implementation

- **Only upload and `/v1/about` are implemented.** File listing, deletion,
  folders, subtitles, and Google Drive import are left to the API and dashboard.
- **The API key is sent in the upload URL path.** This is the documented
  contract. The plugin uses HTTPS; the plain-HTTP form shown in the docs is not
  used.
- **`urlIframe` is ignored.** manyup reports the `abysscdn.com/?v={slug}` watch
  page rather than the player iframe URL.
- **No local size/quota pre-check.** `/v1/about` reports `maxUploadSize` and the
  remaining daily quota, but the plugin lets the upload endpoint enforce them;
  the limits can change per account.
- **No resumable upload.** Abyss accepts one multipart POST, so a retry starts
  the whole upload again.
- **The API key is account-wide.** Uploads made by manyup appear in the
  account's Abyss dashboard and cannot be deleted through manyup.
