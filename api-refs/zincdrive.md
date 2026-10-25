# ZincDrive — API reference

> Reference for the API surface manyup uses, derived from the implemented client.
> Official site: <https://zincdrive.com>.

| | |
|---|---|
| **API base** | `https://zincdrive.com/api/v1` |
| **Auth** | **Required** — `X-API-Key` header |
| **Upload method** | S3 presigned `PUT` + confirm |
| **Max file size** | 10 GB |

---

## Authentication

Every API call carries the account API key in a header — **not** a Bearer token:

```http
X-API-Key: {api_key}
```

---

## Upload flow

### Step 1 — sign

`POST /s3/sign` with `Content-Type: application/x-www-form-urlencoded`:

```
filename=myfile.zip
```

```json
{
  "url": "https://s3.../bucket/key?X-Amz-...",
  "method": "PUT",
  "headers": {
    "Content-Type": "application/zip",
    "x-amz-acl": "private"
  },
  "key": "uploads/abc123/myfile.zip"
}
```

`url` and `key` are both required. `headers` must be applied verbatim to the PUT — the
signature covers them.

### Step 2 — PUT the file

`PUT {url}` with the raw file body and the returned headers.

```http
PUT {presigned url}
Content-Type: application/zip      # from sign response
Content-Length: {size}
```

If the sign response specifies no `Content-Type`, manyup sends
`application/octet-stream`.

### Step 3 — confirm

`POST /s3/confirm` as `application/x-www-form-urlencoded`:

| Field | Value |
|---|---|
| `key` | from the sign response |
| `filename` | original filename |
| `size` | byte length |
| `folder` | optional folder id |
| `visibility` | `1` = public (default), sent explicitly |

```json
{
  "type": "file",
  "shared_id": "...",
  "download_link": "https://zincdrive.com/f/abc123"
}
```

`download_link` is the public URL.

---

## Errors

Sign and confirm decode JSON even on failure; a missing `url`, `key` or `download_link`
is treated as an error. The S3 `PUT` surfaces its own status and body.

---

## manyup integration notes

`internal/services/zincdrive.go`:

- `RequiredCredentials()` returns `["API_TOKEN"]`.
- Applies every header from the sign response to the PUT, then falls back to
  `application/octet-stream` when none was given.
- Sends `visibility=1` by default; `folder_id` and `visibility` can be overridden through
  the service `Options` map.
- Returns `download_link`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (**required**; sent as `X-API-Key`).
