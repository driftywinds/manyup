# GoFile — API reference

> Reference for the API surface manyup uses, derived from the implemented client.
> Official docs: <https://gofile.io/api>.

| | |
|---|---|
| **Upload URL** | `https://upload.gofile.io/uploadfile` |
| **Public URL** | `https://gofile.io/d/{code}` |
| **Auth** | Optional — Bearer token |
| **Upload method** | `multipart/form-data` POST |
| **Max file size** | Unlimited |

GoFile accepts anonymous guest uploads. A token attaches the upload to an account, which
keeps it manageable and prevents guest expiry.

---

## Authentication

```http
Authorization: Bearer {gofile_token}
```

Obtain the token from the GoFile account API. It is optional for manyup.

---

## Upload

`POST https://upload.gofile.io/uploadfile` as `multipart/form-data`.

| Field | Content |
|---|---|
| `file` | the file bytes (required) |
| `folderId` | destination folder id (optional) |

```bash
curl -X POST "https://upload.gofile.io/uploadfile" \
  -H "Authorization: Bearer $TOKEN" \
  -F "file=@myfile.zip" \
  -F "folderId=abc123"
```

### Response

```json
{
  "status": "ok",
  "data": {
    "id": "...",
    "type": "file",
    "name": "myfile.zip",
    "parentFolder": "...",
    "parentFolderCode": "abc123",
    "downloadPage": "https://gofile.io/d/abc123",
    "code": "abc123",
    "size": 12345678,
    "md5": "...",
    "mimetype": "application/zip",
    "createTime": 1700000000,
    "servers": ["store1"]
  }
}
```

`status` must be `"ok"`; anything else is an error.

### Download URL

Prefer `data.downloadPage`. Fall back to `https://gofile.io/d/{code}`, then
`https://gofile.io/d/{parentFolderCode}`.

---

## Errors

Errors are reported through `status != "ok"` rather than HTTP status alone, so a `200`
with a non-`ok` status is still a failure.

---

## manyup integration notes

`internal/services/gofile.go`:

- Builds a streaming `multipart/form-data` body through an `io.Pipe`; the file is never
  buffered in memory. A trailing `folderId` field is written after the file part when
  configured.
- Rejects a response whose `status` is not `"ok"` or whose `data` is empty.
- Prefers `downloadPage`, then `code`, then `parentFolderCode`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (optional).
