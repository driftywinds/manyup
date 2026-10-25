# FuckingFast — API reference

> Verified against the live service on 2026-10-25. The upload flow was exercised
> end to end; response bodies below are real.

| | |
|---|---|
| **Site** | `https://fuckingfast.net` |
| **Upload base** | `https://w.fuckingfast.net` |
| **Public URL** | `https://fuckingfast.net/{fileId}` |
| **Auth** | Optional — `Authorization: Bearer {accountId}` |
| **Upload method** | Raw `PUT` of file bytes |
| **Max file size** | No documented limit |

FuckingFast speaks the same protocol as BuzzHeavier: PUT the raw bytes, get back a
`{code, data}` envelope, and the public link is built from `data.id`.

---

## Authentication

Anonymous uploads are accepted with no headers at all. Supplying an account id ties the
upload to an account and enables the directory/file-manager endpoints:

```http
Authorization: Bearer YOUR_ACCOUNT_ID
```

`GET /api/account` and `GET /api/fs` answer `401 {"code":401,"error":"Unauthorized"}`
without it. `GET /api/locations` is public.

---

## Upload

`PUT https://w.fuckingfast.net/{name}` — the body **is** the file.

| Variant | Request |
|---|---|
| default location | `PUT /{name}` |
| into a user directory | `PUT /{parentId}/{name}` |
| specific storage location | `PUT /{name}?locationId={locationId}` |
| attach a note | `PUT /{name}?note={base64(note)}` |

- `name` is required and limited to **500 characters**.
- `note` is base64-encoded text, max 500 characters, shown under the download link.
- `locationId` comes from `GET /api/locations`.

```bash
curl -#o - -T "sample.mp4" "https://w.fuckingfast.net/sample.mp4" | cat
```

### Response — HTTP `201`

```json
{
  "code": 201,
  "data": {
    "id": "q278hygzt8s3",
    "name": "manyup-probe-1792913530829.txt",
    "isDirectory": false,
    "locationId": "5urmb0o0baxx",
    "size": 22,
    "expiry": "2026-10-29T07:32:13.393933Z",
    "views": 0,
    "downloads": 0,
    "sha256": "e307240ad1d4696d7f6290707b90bc70ed01ed7ca499ac6eeeb49152fddb869c",
    "md5": "f13221f755ab607ec8489740116227b1",
    "note": "",
    "createdAt": "2026-10-25T07:32:13.393933Z"
  }
}
```

Verified behaviours:

- The status is **`201`** and `code` mirrors it (`201`), unlike BuzzHeavier's `0`.
- `locationId` is assigned by the server when not requested.
- `?note=` is accepted and echoed back **decoded** in `data.note`.
- Uploading the same filename twice produces two different ids — no overwrite, no dedupe.
- Anonymous uploads expire (the `expiry` field above was four days out).

### Public link

```
https://fuckingfast.net/{data.id}
```

> The public pages sit behind a **Cloudflare browser challenge**: a plain HTTP GET
> returns `403` with a "Just a moment..." page, for any user agent. Browsers pass it
> normally, so this only affects automated verification — the link itself is correct.

---

## Other endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/locations` | list storage locations (public) |
| `GET` | `/api/account` | account info |
| `GET` | `/api/fs` | root directory |
| `GET` | `/api/fs/{directoryId}` | directory contents |
| `POST` | `/api/fs/{parentDirectoryId}` | create directory (`{"name": "..."}`) |
| `PATCH` | `/api/fs/{id}` | rename directory or file (`{"name": "..."}`) |
| `PUT` | `/api/fs/{id}` | move (`{"parentId": "..."}`) or add note (`{"note": "..."}`) |
| `DELETE` | `/api/fs/{directoryId}` | delete a directory and its contents |

---

## Errors

Errors use the same envelope with an `error` string:

```json
{ "code": 401, "error": "Unauthorized" }
```

| Status | Meaning | Retry? |
|---|---|---|
| `401` | missing/invalid account id | no |
| `413` | file too large (undocumented ceiling) | no |
| `429` | rate limited | **yes** |
| `5xx` | server error | **yes** |

---

## manyup integration notes

`internal/services/fuckingfast.go`:

- Streams the reader as the PUT body, setting `Content-Length` when the size is known and
  `Content-Type: application/octet-stream`.
- URL construction: `/{name}`, or `/{parentId}/{name}` when the `parentId` option is set;
  `locationId` and `note` are added as query parameters. A `parentId` upload suppresses
  `locationId`, because the directory already determines the location.
- `note` is configured as plain text and base64-encoded on the wire.
- Sends `Authorization: Bearer {API_TOKEN}` only when a token is configured.
- Requires `data` and a non-empty `data.id`; the returned URL is
  `https://fuckingfast.net/{id}`.
- Non-2xx responses go through `statusError`, so `status <code>` is present in the error
  and 429/5xx stay retryable.
- `VerifyCredentials` calls `GET /api/account` with the token.
- `SupportsLargeUpload()` is `true`; no client-side size check.

Credential: `API_TOKEN` (optional).
