# DataNodes — API reference

> Reference for the API surface manyup uses. Official docs:
> <https://datanodes.to/pages/api> (sign-in required for the full page; the public
> landing page confirms the API is JSON-over-HTTP and requires a personal API key).

| | |
|---|---|
| **Site / API base** | `https://datanodes.to` |
| **Upload server resolver** | `https://datanodes.to/api/upload/server` |
| **Public URL** | `https://datanodes.to/{fileCode}` |
| **Auth** | **Required** — personal API key |
| **Upload method** | Two-step: resolve server, then `multipart/form-data` POST |
| **Max file size** | 3 GB free / unlimited premium |

---

## Authentication

Every request carries the personal API key. For the upload-server call it is a query
parameter:

```
GET https://datanodes.to/api/upload/server?key={api_key}
```

Keys are generated on the account page. Uploads are performed as `utype=prem`
(premium-style session) and the account's plan decides the real ceiling.

---

## Upload flow

### Step 1 — resolve an upload server and session

```
GET /api/upload/server?key={api_key}
```

```json
{
  "msg": "OK",
  "status": 200,
  "sess_id": "abcdef0123456789",
  "result": "https://s12.datanodes.to/upload/01"
}
```

`result` is the upload endpoint; `sess_id` is the session token. Both are required — a
response missing either is invalid.

### Step 2 — POST the file

`POST {result}` as `multipart/form-data`:

| Field | Value |
|---|---|
| `sess_id` | the session id from step 1 |
| `utype` | `prem` |
| `file_0` | the file bytes, with its original filename |

```bash
curl -X POST "$UPLOAD_SERVER" \
  -F "sess_id=$SESS_ID" \
  -F "utype=prem" \
  -F "file_0=@myfile.zip"
```

### Response

A **JSON array**; the first element is the uploaded file:

```json
[
  {
    "file_code": "abc123def",
    "file_status": "OK"
  }
]
```

`file_status` must be `"OK"`. The public link is:

```
https://datanodes.to/{file_code}
```

---

## Errors

| Signal | Meaning |
|---|---|
| non-2xx on step 1 | bad API key or service unavailable |
| empty `sess_id` or `result` | invalid resolver response |
| `file_status != "OK"` | upload rejected |
| empty array | upload rejected without detail |

---

## manyup integration notes

`internal/services/datanodes.go`:

- `RequiredCredentials()` returns `["API_TOKEN"]`, so the uploader skips the service with
  a warning when it is unset rather than failing the whole run.
- Streams `file_0` through an `io.Pipe`, so multi-gigabyte files never enter memory.
- Writes `sess_id` and `utype` fields before the file part.
- Validates `len(entries) > 0`, `file_status == "OK"` and a non-empty `file_code`.
- Returns `https://datanodes.to/{file_code}`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (**required**).
