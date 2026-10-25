# VikingFile — API reference

> Reference for the API surface manyup uses, sourced from the vendor's own API notes
> (`vikingfile.txt`, captured 2026-10-25) and checked against
> `internal/services/vikingfile.go`. Official site: <https://vikingfile.com>.

| | |
|---|---|
| **Site** | `https://vikingfile.com` |
| **Endpoints** | `POST https://vikingfile.com/api/get-upload-url`, `POST https://vikingfile.com/api/complete-upload` |
| **Public URL** | `https://vikingfile.com/f/{hash}` (the `url` field in the completion response) |
| **Auth** | None for anonymous uploads; `user` is a user hash, sent empty for anonymous |
| **Upload method** | Chunked presigned `PUT` + multipart completion |
| **Max file size** | Not documented by the vendor; the plugin enforces no limit |

---

## Upload flow

### Step 1 — request upload URLs

`POST https://vikingfile.com/api/get-upload-url` with the single `size` parameter
(size of the file in bytes).

```json
{
  "uploadId": "ANWA...SE1M",
  "key": "rZ2h9ZqVQi",
  "partSize": 1073741824,
  "numberParts": 3,
  "urls": [
    "https://upload.vikingfile.com/rZ2h9ZqVQi?partNumber=1&uploadId=ANWA...",
    "https://upload.vikingfile.com/rZ2h9ZqVQi?partNumber=2&uploadId=ANWA...",
    "https://upload.vikingfile.com/rZ2h9ZqVQi?partNumber=3&uploadId=ANWA..."
  ]
}
```

`partSize` is in bytes (1 GiB in the vendor example), and each entry in `urls` already
carries a 1-based `partNumber` and the `uploadId`.

### Step 2 — PUT each part

`PUT` each URL from `urls` with that part's raw bytes. The vendor documents no request
headers for this call; it only specifies the response:

- a `2xx` response carries the part's ETag in the **`ETag` response header**.

The ETag is needed for completion.

### Step 3 — complete

`POST https://vikingfile.com/api/complete-upload`. Vendor parameters:

| Field | Required | Value |
|---|---|---|
| `key` | yes | from step 1 |
| `uploadId` | yes | from step 1 |
| `parts[i][PartNumber]` | yes | part number (1-based); `i` is a 0-based array index |
| `parts[i][ETag]` | yes | ETag from that part's `PUT` response header |
| `name` | yes | filename |
| `user` | yes | user's hash; empty for anonymous upload |
| `path` | no | destination path, e.g. `Folder/My sub folder` |
| `pathPublicShare` | no | public-share id, e.g. `auLXofS1Ku7SrnzA90nOz1` from `https://vikingfile.com/public-upload/auLXofS1Ku7SrnzA90nOz1` |

The vendor example uses `parts[0][PartNumber]=1&parts[0][ETag]=51887c42...`.

Response:

```json
{
  "name": "example.txt",
  "size": 12345,
  "hash": "TPRSfLvcIu",
  "url": "https://vikingfile.com/f/TPRSfLvcIu"
}
```

`url` is the public download link; `hash` identifies the file for the vendor's management
endpoints.

---

## Errors

The vendor notes do not document error responses for these endpoints. manyup treats any
non-2xx status as a failure and surfaces the HTTP status and response body verbatim; the
completion body is parsed as JSON only after a 2xx.

---

## manyup integration notes

`internal/services/vikingfile.go`:

- Sends `size` form-encoded (`size=<bytes>`, `Content-Type: application/x-www-form-urlencoded`)
  to `get-upload-url`; an empty `urls` array is an error.
- Uploads one part per entry in `urls`, in order. Part `i` (0-based) covers the byte range
  starting at `i*partSize`, capped at the remaining file size for the last part.
- Streams each part through an `io.Pipe` so disk reads overlap with the network upload, and
  sets `Content-Length` plus `Content-Type: application/octet-stream` on the `PUT`.
- Requires a non-empty `ETag` response header on every part; a missing ETag is an error.
- Completes with `multipart/form-data` fields `key`, `uploadId`,
  `parts[i][PartNumber]`, `parts[i][ETag]`, `name` (`filepath.Base(filename)`), `user` from
  the `API_TOKEN` credential (empty when unset), and `path` from the optional `path`
  option when non-empty.
- Requires a non-empty `url` in the completion response; returns that URL and the reported
  `size`.
- `SupportsLargeUpload()` is `true`; `RequiredCredentials()` returns none.

Credential: `API_TOKEN` (optional; used verbatim as the `user` hash).

---

## ⚠ Divergence from the implementation

- `complete-upload` documents an optional `pathPublicShare` field; the plugin never sends it
  (only `path` is configurable).
