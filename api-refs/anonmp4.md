# AnonMP4 — API reference

> Reference for the API surface manyup uses, derived from the official
> documentation (<https://anonmp4.to/api-docs>, retrieved 2026-11-04), the API
> host (<https://anonmp4api.xyz/>), and `internal/services/anonmp4.go`.

| | |
|---|---|
| **Site** | `https://anonmp4.to` |
| **API base** | `https://anonmp4api.xyz` |
| **Endpoints used** | `POST /upload` |
| **Public URL** | `watch_url`, e.g. `https://anonmp4.to/v/{video_id}` |
| **Auth** | None — anonymous uploads |
| **Upload method** | One streaming `multipart/form-data` POST |
| **Max file size** | 20 GB documented; the error example reports a 53,687,091,200-byte cap (50 GiB), so treat the server response as authoritative |
| **Retention** | Not documented; the API exposes no delete/expiry metadata in the upload response |

AnonMP4 is a video host, not a general file host. Uploads must be video files
(MP4, AVI, MKV, MOV, WebM, and other `video/*` formats). There is no account,
no API key, and no credential to configure.

---

## Authentication

None. Every request can be made anonymously:

```http
POST /upload HTTP/1.1
Host: anonmp4api.xyz
Content-Type: multipart/form-data; boundary=…
```

The plugin never sends an `Authorization` header.

---

## Upload

```http
POST https://anonmp4api.xyz/upload HTTP/1.1
Content-Type: multipart/form-data; boundary=----manyup…
```

### Request

`multipart/form-data` with one required file field:

| Field | Type | Required | Description |
|---|---|---|---|
| `file` | file | yes | Video file, up to 20 GB, `video/*` MIME type |

The official examples send only that field. Optional metadata/privacy settings
are mentioned in the overview but are not documented as request fields, so the
plugin sends only `file`.

### Success response

`200 OK` with a JSON body:

```json
{
  "success": true,
  "video_id": "NLEusIzEvLs3s3B",
  "title": "BigBuckBunny",
  "thumbnail": "https://cdn.anonmp4.to/thumbs/NLEusIzEvLs3s3B.webp",
  "watch_url": "https://anonmp4.to/v/NLEusIzEvLs3s3B",
  "embed_url": "https://anonmp4.to/embed/NLEusIzEvLs3s3B",
  "delete_url": "https://anonmp4.to/delete?id=NLEusIzEvLs3s3B&hash=uniqehash",
  "upload_date": "2026-01-08T10:30:00Z",
  "message": "File received and queued for processing"
}
```

Use `watch_url` as the public result. The response says the file is **queued for
processing**, and the separate `GET /info/{video_id}` endpoint exposes the
processing state, so a freshly uploaded video may not be playable immediately.

The upload response is the only place `delete_url` is returned; it is the only
way to delete an anonymous upload. manyup does not persist it.

### Error response

Non-2xx responses use one envelope:

```json
{
  "success": false,
  "error": {
    "code": "E_INVALID_SIZE",
    "message": "File size exceeds the 20GB limit",
    "details": {
      "max_size": 53687091200,
      "received_size": 107374182400
    }
  }
}
```

| Status | Meaning |
|---|---|
| 200 | Success |
| 400 | Bad request — invalid parameters |
| 404 | Not found — video does not exist |
| 413 | Payload too large — file exceeds the size limit |
| 415 | Unsupported media type — not a supported video format |
| 429 | Too many requests — rate limit exceeded |
| 500 | Internal server error |

| Code | Meaning |
|---|---|
| `E_NO_FILE` | No file was provided in the upload request |
| `E_INVALID_SIZE` | File exceeds the size limit |
| `E_INVALID_TYPE` | File is not a supported video format |
| `E_UPLOAD_FAILED` | Upload processing failed |
| `E_HANDLER_FAILED` | Upload handler failed |
| `E_VIDEO_NOT_FOUND` | Requested video does not exist |

A proxy or maintenance page can return an HTML body instead of the JSON
envelope. The plugin keeps the HTTP status in the error text so the uploader's
retry classifier still recognises `429` and `5xx`.

---

## Video info

```http
GET https://anonmp4api.xyz/info/NLEusIzEvLs3s3B
```

`200 OK`:

```json
{
  "success": true,
  "title": "BigBuckBunny",
  "duration": "09:56",
  "watch_url": "https://anonmp4.to/v/NLEusIzEvLs3s3B",
  "embed_url": "https://anonmp4.to/embed/NLEusIzEvLs3s3B",
  "thumbnail": "https://cdn.anonmp4.to/thumbs/NLEusIzEvLs3s3B.webp",
  "upload_date": "2025-12-28 18:01:38",
  "privacy_type": "public",
  "status": "active"
}
```

The plugin does not call this endpoint. It is documented here because it is the
only way to check processing status or retrieve video metadata after an upload.

---

## manyup integration notes

`internal/services/anonmp4.go`:

- **Service name.** `anonmp4`, display name `AnonMP4`. It is registered in
  `internal/services/registry.go` next to the other built-ins.
- **Credentials.** None. `RequiredCredentials()` returns nothing and the plugin
  does not implement `plugin.CredentialVerifier`; `manyup config set anonmp4` is
  never needed. `manyup config select anonmp4` is enough to enable it.
- **Streaming.** The request body is assembled as a prefix (multipart headers),
  the uploader's progress reader, and a suffix (closing boundary), joined with
  `io.MultiReader`. The file is never buffered. `Content-Length` is computed as
  `len(prefix) + size + len(suffix)` and set on the request, so the body is sent
  with a known length instead of chunked transfer encoding. The file section is
  wrapped in `io.LimitReader(reader, size)` so the declared length and the bytes
  on the wire stay in sync.
- **Boundary.** A fresh `----manyup<36 hex chars>` boundary is generated with
  `crypto/rand` for every upload.
- **File part.** The field name is `file`; the part filename is
  `filepath.Base(filename)`, with CR/LF replaced by `_` and `\`/`"` escaped.
  The part `Content-Type` is chosen from an explicit video-extension map
  (`.mp4` → `video/mp4`, `.mkv` → `video/x-matroska`, `.webm` → `video/webm`,
  and so on), then from the host MIME registry when it returns a `video/*`
  type, and finally `application/octet-stream`.
- **Pre-flight checks.** Only empty files are rejected locally. The 20 GB cap
  and the supported-format check are enforced by the server through the
  `413`/`415` responses; the plugin does not duplicate the documented limit in
  code because the published `max_size` example is inconsistent with the 20 GB
  prose.
- **Timeout.** A 20 GB POST can legitimately take longer than the shared HTTP
  client's five-minute timeout, so the plugin uses a copy of the shared client
  with its overall timeout removed. Context cancellation (Ctrl+C) still aborts
  the transfer.
- **Success.** The result URL is `watch_url`; the declared size is reported as
  `UploadResult.Size` because the upload response does not include a size field.
  The `video_id`, `title`, thumbnail, embed URL, delete URL, and upload date are
  not part of `plugin.UploadResult` and are discarded.
- **Errors.** A non-2xx response is formatted as
  `anonmp4: status NNN: CODE: message (details)`, preserving the status code the
  uploader uses to decide whether to retry. A `200` with `success: false` is
  reported as `anonmp4: CODE: message`. A malformed response is reported with a
  short body excerpt.
- **Retries.** The plugin itself does not retry. The uploader retries `429`,
  `5xx`, timeouts, and dropped connections; `400`, `404`, `413`, and `415` fail
  fast.
- **`SupportsLargeUpload()`** is `true`.

Manual checks (replace the file with a real video):

```bash
# Upload
curl -X POST https://anonmp4api.xyz/upload -F "file=@video.mp4"

# Inspect the resulting video
curl https://anonmp4api.xyz/info/VIDEO_ID
```

There is no live test for this service: exercising it writes a real video to a
third-party host, and the repository has no small, redistributable video
fixture. The unit tests use a local `httptest` server that parses the multipart
body and verifies the exact `Content-Length`, field name, filename, MIME type,
and payload.

---

## Divergence from the implementation

- **`GET /info/{video_id}` is not used.** manyup reports the watch URL from the
  upload response and does not poll for processing status, duration, thumbnail,
  or privacy.
- **`delete_url` is discarded.** `plugin.UploadResult` has no field for it, and
  manyup does not persist it. An anonymous upload therefore cannot be deleted
  through manyup after the command exits.
- **The 20 GB limit is not pre-checked.** The documented prose says 20 GB while
  the error example reports a 50 GiB `max_size`; the server's response is the
  only reliable limit, so the plugin lets it answer.
- **Non-video files are not rejected locally.** A `.zip` upload fails with the
  server's `415 E_INVALID_TYPE` rather than a local validation error. This keeps
  the plugin from rejecting a format the server might add support for.
- **No resumable uploads.** The API accepts one multipart POST; there is no
  chunked or resumable protocol to implement. A retry starts the whole upload
  again.
- **Optional metadata is not sent.** The documentation mentions “optional
  metadata and privacy settings” but only documents the `file` field, so the
  plugin sends exactly that.
