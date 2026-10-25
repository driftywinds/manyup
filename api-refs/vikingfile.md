# VikingFile — API reference

> Reference for the API surface manyup uses, derived from the implemented client.
> Official site: <https://vikingfile.com>.

| | |
|---|---|
| **Upload URL resolver** | `https://vikingfile.com/api/get-upload-url` |
| **Complete URL** | `https://vikingfile.com/api/complete-upload` |
| **Auth** | Optional — user hash |
| **Upload method** | Chunked presigned `PUT` + multipart completion |
| **Max file size** | Unlimited |

---

## Upload flow

### Step 1 — request upload URLs

`POST https://vikingfile.com/api/get-upload-url` with
`Content-Type: application/x-www-form-urlencoded`:

```
size={bytes}
```

```json
{
  "uploadId": "...",
  "key": "...",
  "partSize": 10485760,
  "numberParts": 3,
  "urls": [
    "https://storage.../part1",
    "https://storage.../part2",
    "https://storage.../part3"
  ]
}
```

`numberParts` must match `len(urls)`; an empty `urls` array is an error.

### Step 2 — PUT each part

`PUT {url}` with the raw bytes of that slice:

```http
Content-Type: application/octet-stream
Content-Length: {partSize}
```

A `2xx` response carries the part `ETag` in the **`ETag` response header**. A missing
`ETag` is an error — it is required for completion.

Parts are computed from `partSize`; the last part is truncated to the remaining bytes.

### Step 3 — complete

`POST https://vikingfile.com/api/complete-upload` as `multipart/form-data`:

| Field | Value |
|---|---|
| `key` | from step 1 |
| `uploadId` | from step 1 |
| `name` | original filename |
| `user` | user hash, empty for anonymous |
| `path` | optional destination path |
| `parts[{i}][PartNumber]` | 1-based index |
| `parts[{i}][ETag]` | ETag captured from each part PUT |

```json
{
  "name": "myfile.zip",
  "size": 12345678,
  "hash": "...",
  "url": "https://vikingfile.com/f/abc123"
}
```

`url` is the public download link.

---

## Errors

Every step returns its failure as a non-2xx status with a textual body, which manyup
surfaces verbatim. There is no JSON error envelope.

---

## manyup integration notes

`internal/services/vikingfile.go`:

- Streams each part through an `io.Pipe` so disk reads overlap with the network upload.
- Requires an `ETag` header on every part response.
- Sends `user` from `API_TOKEN` (empty when unset).
- Optional `path` comes from the service `Options` map.
- Returns `url` from the completion response and the reported `size`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (optional; treated as a user hash).
