# Streamtape — API reference

> Reference for the API surface manyup uses, derived from the official
> Streamtape API document (retrieved 2026-11-08) and checked against
> `internal/services/streamtape.go` and the live service.

| | |
|---|---|
| **Site** | `https://streamtape.com` |
| **API base** | `https://api.streamtape.com` |
| **Endpoints used** | `GET /file/ul`; `POST` to the returned upload URL; `GET /account/info` for the credential check |
| **Auth** | API login + API key, passed as query parameters |
| **Upload method** | `GET /file/ul` → `multipart/form-data` POST with field `file1` |
| **Max file size** | Not documented; account storage applies |
| **Rate/bandwidth errors** | `509` when account bandwidth is exceeded |

Streamtape is a video host. Uploads are tied to an account. The public watch URL
is returned directly by the upload response in the form
`https://streamtape.com/v/{id}/{filename}`.

> **Do not commit the API login or key.** Set them with
> `manyup config set streamtape USERNAME <login>` and
> `manyup config set streamtape API_TOKEN <key>`. Both values are required.

---

## Authentication

The API login and API key are found in the Streamtape user panel under
**Account Settings**. Every request used by manyup sends them as query
parameters:

```http
GET https://api.streamtape.com/file/ul?login={login}&key={key}
```

HTTPS is used for every call. The service redirects HTTP to HTTPS; manyup never
sends credentials over plain HTTP.

### Account check

```http
GET https://api.streamtape.com/account/info?login={login}&key={key}
```

```json
{
  "status": 200,
  "msg": "OK",
  "result": {
    "apiid": "<api key>",
    "email": "user@gmail.com",
    "signup_at": "2019-12-31 23:59:59"
  }
}
```

manyup uses this only to verify the login/key pair. It does not use the account
email, storage totals, or reward fields.

---

## Response envelope

Streamtape wraps every response as:

```json
{
  "status": 200,
  "msg": "OK",
  "result": {}
}
```

The HTTP status is often also `200` even when the API status is an error, so
the plugin parses the JSON `status` field instead of trusting the HTTP line.
`msg` is for display; code checks `status`.

| API status | Meaning |
|---|---|
| `200` | Success |
| `400` | Bad request, including malformed parameters |
| `403` | Permission denied: wrong login/key or a file that belongs to another account |
| `404` | File not found |
| `451` | Unavailable for legal reasons |
| `509` | Account bandwidth exceeded; try later |
| `5xx` | Server error |

For credential verification, `400`/`403` reject the login/key. `509`, `429`,
and `5xx` are treated as inconclusive so a valid credential is not discarded
because the account is temporarily out of bandwidth or the host is down.

---

## Upload flow

### 1. Get an upload URL

```http
GET https://api.streamtape.com/file/ul?login={login}&key={key}
```

Optional query parameters:

| Name | Description |
|---|---|
| `folder` | Folder ID to upload into. manyup exposes this as the `folder` service option. |
| `sha256` | Expected SHA-256. If supplied, the upload fails when the hash does not match. manyup does not supply it. |
| `httponly` | Restrict the returned upload URL to HTTP. manyup does not use this. |

Response:

```json
{
  "status": 200,
  "msg": "OK",
  "result": {
    "url": "https://860125117.tapecontent.net/xfsproupload/<short-lived-token>",
    "valid_until": "2019-12-31 23:59:59"
  }
}
```

The returned URL is short-lived and is used immediately.

### 2. Upload the file

```http
POST https://860125117.tapecontent.net/xfsproupload/<short-lived-token>
Content-Type: multipart/form-data; boundary=----manyup…
Accept: application/json

------manyup…
Content-Disposition: form-data; name="file1"; filename="video.mp4"
Content-Type: video/mp4

<raw file bytes>
------manyup…--
```

The field name is **`file1`**. manyup builds the multipart body with an exact
`Content-Length` and streams the file through `io.MultiReader`; the file is not
buffered in memory.

Success response:

```json
{
  "status": 200,
  "msg": "Upload finished!",
  "result": {
    "url": "https://streamtape.com/v/W3MQGg1Z4WFgq1/video.mp4",
    "sha256": "a1c8f913d2a121715ddc4fdbf09c012f5a6dadb08dace56881b1ab1013f2d576",
    "name": "video.mp4",
    "size": "2242",
    "content_type": "video/mp4",
    "id": "W3MQGg1Z4WFgq1"
  }
}
```

The plugin reports `result.url`. The response also contains `result.id` and
`result.name`; the public watch URL has the shape
`https://streamtape.com/v/{id}/{name}`.

A non-video file, malformed multipart request, or server-side processing problem
can produce:

```json
{
  "status": 500,
  "msg": "error on our side",
  "result": null
}
```

Because that is a `5xx`, the uploader retries it according to its normal retry
policy. A `403` upload response is not retried.

---

## Other endpoints (not used by manyup)

Streamtape also provides account/download tickets, remote upload, folder and
file management, conversion listings, and thumbnail retrieval. manyup only
implements the upload flow and the account check. Deleting a file is available
via `GET /file/delete`, but manyup does not expose a delete command for any
service.

---

## manyup integration notes

`internal/services/streamtape.go`:

- **Service name.** `streamtape`, display name `Streamtape`. Registered in
  `internal/services/registry.go`.
- **Credentials.** `USERNAME` (API login) and `API_TOKEN` (API key). Both are
  required; the uploader skips the service if either is missing.
- **Credential check.** `GET /account/info?login=…&key=…`. HTTP `401`/`403` and
  API `400`/`403` reject the pair; `509`/`5xx` are inconclusive. Because the two
  values are configured separately, `config set` verifies with the other stored
  value when it exists. The first value is saved with a "could not be verified"
  note; setting the second verifies the pair.
- **Folder option.** `manyup config set streamtape folder <folder-id>` is
  passed as the optional `folder` parameter to `/file/ul`.
- **Upload.** Two requests: `GET /file/ul`, then a streaming multipart POST to
  the returned URL with field `file1` and an exact `Content-Length`.
- **Timeout.** The plugin uses a copy of the shared client with its overall
  timeout removed so large videos can finish. Context cancellation still
  aborts.
- **Response.** `result.url` is required and returned as the upload URL. The
  declared file size is reported because the response's `size` is a string and
  is only informational.
- **Errors.** HTTP errors go through `statusError`; API errors become
  `streamtape: status NNN: msg`, preserving the status for retry
  classification. `509` and `5xx` are retryable; `403` is not.
- **Pre-flight checks.** Only empty files are rejected locally. There is no
  documented per-file limit in the API, so size and storage limits are left to
  the server.
- **`SupportsLargeUpload()`** is `true`.

Config example:

```bash
manyup config set streamtape USERNAME your_api_login
manyup config set streamtape API_TOKEN your_api_key
manyup config select streamtape
manyup upload video.mp4
```

The live endpoint was verified with a small valid MP4: `/file/ul` returned an
upload URL, the `file1` multipart POST returned a `streamtape.com/v/...` link,
and the probe file was deleted with `/file/delete`.

---

## Divergence from the implementation

- **Only upload and `/account/info` are implemented.** Download tickets, remote
  upload, folders, file management, conversion listings, and thumbnails are
  left to the API and dashboard.
- **Malformed multipart requests and non-video files can return API `500`.**
  The service describes this as "error on our side"; the plugin treats it as a
  retryable server error rather than attempting to distinguish content errors.
- **The API does not document a per-file maximum.** manyup does not impose one.
- **No resumable upload.** Streamtape accepts one multipart POST per file, so a
  retry starts the upload again.
- **No SHA-256 preflight.** The API can enforce a supplied hash, but manyup
  would have to read the whole file twice to provide it and does not.
- **No delete/list commands.** The service supports them, but they are not part
  of manyup's upload-only interface.
