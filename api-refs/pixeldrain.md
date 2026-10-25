# PixelDrain — API reference

> Reference for the API surface manyup uses, derived from the implemented client.
> Official docs: <https://pixeldrain.com/api>.

| | |
|---|---|
| **API base** | `https://pixeldrain.com/api` |
| **Public URL** | `https://pixeldrain.com/u/{fileId}` |
| **Auth** | Optional — HTTP Basic, API key as password |
| **Upload method** | Raw `PUT` of file bytes |
| **Max file size** | Plan-dependent |

---

## Authentication

PixelDrain uses **HTTP Basic auth with an empty username** and the API key as the
password:

```http
Authorization: Basic base64(":" + api_key)
```

Anonymous uploads are accepted when no key is supplied, but the key raises transfer
limits and ties files to the account.

---

## Upload

`PUT https://pixeldrain.com/api/file/{filename}`

The body is the raw file.

```bash
curl -X PUT "https://pixeldrain.com/api/file/myfile.zip" \
  -u ":${PIXELDRAIN_API_KEY}" \
  --data-binary "@myfile.zip"
```

### Response

```json
{
  "success": true,
  "id": "abc123",
  "name": "myfile.zip",
  "size": 12345678
}
```

On failure `success` is `false` and the body carries:

```json
{ "success": false, "value": "machine_code", "message": "human readable" }
```

### Download URL

```
https://pixeldrain.com/u/{id}
```

---

## Errors

| Signal | Meaning |
|---|---|
| `success: false` | API-level rejection |
| `value` | machine-readable error code |
| `message` | human-readable explanation |
| HTTP `4xx/5xx` | transport-level rejection; body may still be JSON |

---

## manyup integration notes

`internal/services/pixeldrain.go`:

- Streams the reader as the PUT body and sets `Content-Length` when the size is known.
- Sends `Content-Type: application/octet-stream`.
- Adds the Basic auth header only when `API_TOKEN` is present.
- Treats `success == false` as an error and appends `value` / `message` to it.
- Returns `https://pixeldrain.com/u/{id}`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (optional).
