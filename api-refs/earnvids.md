# Earnvids — API reference

> Reference for the API surface manyup uses, derived from the Earnvids API
> document (retrieved 2026-11-08) and checked against
> `internal/services/earnvids.go` and the live service.

| | |
|---|---|
| **Site** | `https://earnvids.com` |
| **API base** | `https://earnvidsapi.com` |
| **Endpoints used** | `GET /api/upload/server`, `POST` to the returned upload server, `GET /api/file/list`, `GET /api/account/info` |
| **Auth** | API key; query parameter on API calls, `key` form field on upload |
| **Upload method** | Upload-server discovery → `multipart/form-data` POST |
| **Max file size** | Not documented; account storage applies |
| **Public link** | Resolved from the file list; the live account used `https://morencius.com/file/{file_code}` |

Earnvids is a video host. Uploads are tied to an account API key. The API document
describes older `xvs.tt` links; the live service currently returns account-specific
public domains, so manyup does not hardcode a watch URL.

---

## Authentication

The API key is sent as the `key` query parameter on API requests:

```http
GET https://earnvidsapi.com/api/account/info?key={key}
```

The API returns this envelope, with the real status in the JSON body:

```json
{
  "msg": "OK",
  "server_time": "2021-09-11 04:30:07",
  "status": 200,
  "result": {}
}
```

For uploads, the same key is sent as a multipart form field:

```text
key=<api key>
```

manyup does not use the old HTML form redirect mode and always requests JSON.

### Account info

```http
GET https://earnvidsapi.com/api/account/info?key={key}
```

```json
{
  "msg": "OK",
  "server_time": "2021-09-11 04:30:07",
  "status": 200,
  "result": {
    "files_total": "31",
    "storage_left": 1288483337,
    "login": "user",
    "email": "user@example.com",
    "premium_expire": "2022-10-15 04:46:59",
    "balance": "108.00000",
    "premium": 1,
    "storage_used": "685101"
  }
}
```

manyup uses this only to verify the key.

---

## Upload flow

### 1. Get an upload server

```http
GET https://earnvidsapi.com/api/upload/server?key={key}
```

```json
{
  "msg": "OK",
  "server_time": "2021-08-11 04:29:54",
  "status": 200,
  "result": "https://s1.myvideo.com/upload/01"
}
```

The host is account- and session-specific. The live service returned CDN hosts
such as `https://<random>.dramiyos-cdn.com/upload/01`.

### 2. Upload the file

```http
POST https://<upload-host>/upload/01
Content-Type: multipart/form-data; boundary=----manyup…
Accept: application/json
```

Multipart fields:

| Field | Value |
|---|---|
| `key` | API key |
| `file` | The video file |
| `fld_id` | Optional folder ID; manyup exposes this as the `folder` option |

The documented optional fields (`file_title`, `file_descr`, `snapshot`, `cat_id`,
`tags`, `file_public`, `file_adult`, `html_redirect`) are not sent by manyup.
manyup builds the multipart body with an exact `Content-Length` and streams the
file through `io.MultiReader`; the file is not buffered in memory.

Success response:

```json
{
  "msg": "OK",
  "status": 200,
  "files": [
    {
      "filecode": "jh2842jgd9ty",
      "filename": "manyup-probe.mp4",
      "status": "OK"
    }
  ]
}
```

The upload response has a top-level `files` array, not a `result` field. The
public URL is **not** in this response.

A per-file failure is reported with an empty `filecode` and a message in
`status`:

```json
{
  "msg": "OK",
  "status": 200,
  "files": [
    {
      "filecode": "",
      "filename": "manyup-probe.mp4",
      "status": "0:0:0:video is too short"
    }
  ]
}
```

The HTTP status is still `200`, and the API-level `status` is also `200`; the
per-file `files[].status` decides success. Any `filecode`-bearing response with
a non-`OK` per-file status is treated as an upload failure.

### 3. Resolve the public link

The public URL is returned by:

```http
GET https://earnvidsapi.com/api/file/list?key={key}&per_page=100&page=1
```

```json
{
  "msg": "OK",
  "server_time": "2021-08-13 20:35:18",
  "status": 200,
  "result": {
    "files": [
      {
        "file_code": "jh2842jgd9ty",
        "link": "https://morencius.com/file/jh2842jgd9ty",
        "title": "manyup-probe",
        "uploaded": "2026-11-08 05:12:44"
      }
    ]
  }
}
```

manyup reads the first page and matches `file_code`; the newest upload is
normally first. It returns `files[].link` as the upload URL. Do not hardcode
`xvs.tt`, `morencius.com`, or any other public domain: they vary.

---

## Error statuses

The API envelope's `status` values follow the documented set:

| API status | Meaning |
|---|---|
| `200` | Success |
| `400` | Bad request / invalid operation |
| `403` | Permission denied (bad key or file not owned by the account) |
| `404` | File not found |
| `451` | Unavailable for legal reasons |
| `509` | Bandwidth limit exceeded |
| `5xx` | Server error |

manyup treats API `400`/`403` on `/api/account/info` as rejected credentials,
and `509`/`5xx` as inconclusive. Upload-server failures preserve the HTTP status;
per-file upload failures are surfaced as ordinary errors, with storage/database
failures marked retryable and obvious input failures ("video is too short",
"invalid") left permanent.

There is no documented per-file size limit. Upload nodes can reject a video for
being too short or malformed even when the HTTP request succeeds.

---

## Other endpoints (not used by manyup)

Earnvids also documents account stats, folder/file management, file editing,
cloning, deletion, DMCA listing, encodings, and URL uploads. manyup implements
only upload, link lookup, and the account check.

`GET /api/file/delete?key=…&file_code=…` is documented, but a live probe against
the test account returned API `400 "Invalid operation"` after the file had
finished encoding, so manyup does not expose deletion.

---

## manyup integration notes

`internal/services/earnvids.go`:

- **Service name.** `earnvids`, display name `Earnvids`. Registered in
  `internal/services/registry.go`.
- **Credential.** `API_TOKEN`, holding the Earnvids API key. It is required; the
  uploader skips the service when it is missing.
- **Credential check.** `GET /api/account/info?key=…`. HTTP `401`/`403` and API
  `400`/`403` reject; `509`/`5xx` are inconclusive.
- **Upload.** `GET /api/upload/server`, then a streaming multipart POST with
  `key` and `file` (and optional `fld_id`) and an exact `Content-Length`.
- **Public link.** Resolved from `/api/file/list` by matching the returned
  `filecode`; the link is whatever domain the account's list returns.
- **Timeout.** The upload uses the shared client with its whole-request timeout
  removed; context cancellation still aborts.
- **Errors.** HTTP errors use `statusError`; API errors preserve
  `status NNN: msg` for retry classification. Per-file storage/database errors
  are tagged `status 503` so the uploader retries, while "video is too short" and
  invalid-input messages fail permanently.
- **Pre-flight checks.** Only empty files are rejected locally. Bandwidth,
  storage, and video validation are enforced by the server.
- **`SupportsLargeUpload()`** is `true`.

Config example:

```bash
manyup config set earnvids your_api_key
manyup config select earnvids
manyup upload video.mp4
```

The live service was verified with a 12-second MP4: `/api/upload/server`
returned a CDN upload URL, the `key`/`file` multipart POST returned a
`filecode`, and `/api/file/list` returned a working `morencius.com/file/...`
public link.

---

## Divergence from the implementation

- **Only upload, link lookup, and account info are implemented.** Stats,
  folders, file management, clones, encodings, remote URL upload, and deletion
  are left to the API and dashboard.
- **Public domains vary.** The docs' `xvs.tt` examples are not used; the live
  list returned `morencius.com`. manyup uses `files[].link`.
- **Upload success is per-file.** The HTTP and API envelope statuses can both be
  `200` while `files[0].status` reports an error. manyup checks both.
- **No documented per-file limit.** manyup does not impose one.
- **No resumable upload.** A retry starts the upload over.
- **No delete command.** The documented delete returned `Invalid operation` on
  the test account; manyup does not expose it.
