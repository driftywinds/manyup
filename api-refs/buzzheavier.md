# BuzzHeavier — API reference

> Reference for the API surface manyup uses, derived from the implemented client.
> Official site: <https://buzzheavier.com>.

| | |
|---|---|
| **Upload base** | `https://w.buzzheavier.com` |
| **Public file URL** | `https://buzzheavier.com/{fileId}` |
| **Auth** | Optional — account token as Bearer |
| **Upload method** | Raw `PUT` of file bytes |
| **Max file size** | Unlimited |

---

## Authentication

Anonymous uploads are accepted. Supplying a token ties the upload to an account and
raises limits:

```http
Authorization: Bearer {account_token}
```

The token is the account id / API token from the BuzzHeavier account page.

---

## Upload

`PUT https://w.buzzheavier.com/{filename}`

The body **is** the file — raw bytes, not multipart.

```
PUT /myfile.zip
Content-Type: application/octet-stream
Content-Length: <size>
Authorization: Bearer <token>      # optional
```

Optional destination selection:

| Variant | URL |
|---|---|
| default | `PUT https://w.buzzheavier.com/{filename}` |
| into a folder | `PUT https://w.buzzheavier.com/{parentId}/{filename}` |
| into a location | `PUT https://w.buzzheavier.com/{filename}?locationId={locationId}` |

### Response

```json
{
  "code": 0,
  "data": {
    "id": "abc123",
    "name": "myfile.zip",
    "size": 12345678,
    "locationId": "..."
  }
}
```

The public download URL is built from `data.id`:

```
https://buzzheavier.com/{id}
```

---

## Errors

Failures either return a non-2xx status or a body without `data`. There is no documented
`code` enumeration; manyup treats a missing `data` object as an error and surfaces the
raw body.

---

## manyup integration notes

`internal/services/buzzheavier.go`:

- Streams the reader directly as the PUT body (no buffering).
- Sets `Content-Length` from the file stat when available.
- Optional `parentId` / `locationId` come from the service `Options` map.
- Returns `https://buzzheavier.com/{data.id}`.
- `SupportsLargeUpload()` is `true`.

Credential: `API_TOKEN` (optional).
