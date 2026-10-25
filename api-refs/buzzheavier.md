# BuzzHeavier — API reference

> Reference for the API surface manyup uses, derived from the vendor documentation
> (`api-refs/buzzheavier.txt`) and the implemented client. Where the vendor docs are silent,
> the statement is marked as observed behaviour of the implementation.
> Official site: <https://buzzheavier.com>.

| | |
|---|---|
| **Site** | `https://buzzheavier.com` |
| **Upload base** | `https://w.buzzheavier.com` |
| **Public file URL** | `https://buzzheavier.com/{fileId}` |
| **Auth** | Optional — `Authorization: Bearer {accountId}` |
| **Upload method** | Raw `PUT` of file bytes |
| **Max file size** | No documented limit; file name ≤ 500 chars, note ≤ 500 chars |

---

## Authentication

Anonymous uploads are documented and accepted — no header is required.

Authenticated uploads use the account id as the bearer value:

```http
Authorization: Bearer {accountId}
```

The vendor documents `YOUR_ACCOUNT_ID` as the value. Related endpoints:

| Method | Endpoint | Purpose |
|---|---|---|
| `GET` | `https://buzzheavier.com/api/account` | Current authenticated account's information |
| `GET` | `https://buzzheavier.com/api/locations` | File storage locations available to the account (`locationId` values) |

---

## Upload

Single raw `PUT`; the body **is** the file, not multipart.

```
PUT /myfile.zip HTTP/1.1
Host: w.buzzheavier.com
Content-Type: application/octet-stream
Content-Length: <size>
Authorization: Bearer <accountId>      # optional
```

Destination is selected by the URL path or query:

| Variant | Request | Notes |
|---|---|---|
| anonymous / default | `PUT https://w.buzzheavier.com/{name}` | uploads to the default location |
| into user directory | `PUT https://w.buzzheavier.com/{parentId}/{name}` | `parentId` is a directory id under the account |
| to specific location | `PUT https://w.buzzheavier.com/{name}?locationId={locationId}` | `locationId` optional |
| with note | `PUT https://w.buzzheavier.com/{name}?note={base64(note)}` | note is base64-encoded text shown under the download link, optional |

Parameters:

| Param | In | Required | Limit |
|---|---|---|---|
| `name` | path | yes | max 500 chars |
| `parentId` | path | optional | directory id |
| `locationId` | query | optional | storage location id |
| `note` | query | optional | base64-encoded text, max 500 chars |

### Response

The vendor docs do not publish a response schema. The shape below is what the
implementation parses (observed, not vendor-documented):

```jsonc
{
  "code": <integer>,            // status; value not documented by the vendor
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

The vendor docs publish no error catalogue or `code` enumeration.

- An invalid or missing account token on `/api/account` answers `401` with
  `{"code":401,"error":"Unauthorized"}` (observed).
- The upload path is judged by the body: a non-JSON body, or a JSON body without a `data`
  object, is treated as a failure and the status plus raw body are surfaced. HTTP status
  alone is not inspected.

---

## manyup integration notes

`internal/services/buzzheavier.go` — service slug `buzzheavier`, display name `BuzzHeavier`,
description `BuzzHeavier fast file hosting (PUT upload)`.

- **Upload**: one raw `PUT` to `https://w.buzzheavier.com/{filename}`. The filename is
  `filepath.Base(name)` with `url.PathEscape`; the caller's reader streams directly as the
  body (no buffering, no multipart).
- **Destination**: `cfg["parentId"]` becomes
  `PUT https://w.buzzheavier.com/{parentId}/{filename}`; otherwise a non-empty
  `cfg["locationId"]` appends `?locationId=`. If both are set, `parentId` wins.
- **Headers**: `Content-Type: application/octet-stream`; `Content-Length` is set from the
  supplied size when `size > 0`; `Authorization: Bearer {creds["API_TOKEN"]}` is added only
  when the token is non-empty.
- **Response handling**: unmarshals into `{code, data{id,name,size,locationId}}`, requires a
  non-nil `data`, and returns `https://buzzheavier.com/{data.id}`. The reported size is
  `data.size` (server-side), not the local file size.
- **`note`**: never sent — there is no config key for it.
- **Credentials**: `API_TOKEN`, optional (`RequiredCredentials()` returns `nil`; anonymous
  uploads work). No credential means no `Authorization` header.
- **`VerifyCredentials`**: `GET https://buzzheavier.com/api/account` with
  `Authorization: Bearer {API_TOKEN}` (plus `Accept: application/json`). Empty token →
  `ErrCredentialRejected`; 2xx → valid; 401/403 and other 4xx → rejected; 429/5xx →
  inconclusive "could not verify".
- **`SupportsLargeUpload()`**: `true`.
- **Timeout**: `bhAPITimeout` (300 s) is declared in the file but not applied in `Upload`;
  the request uses the shared `httpclient.Get()` client (300 s timeout) plus the caller's
  context.

---

## ⚠ Divergence from the implementation

Maintainer notes — where the vendor docs and the plugin do not line up:

- Vendor documents a `?note={base64}` upload variant; the plugin has no way to send it.
- Vendor calls the bearer value `YOUR_ACCOUNT_ID`; the plugin stores it under the credential
  key `API_TOKEN` (same value, different name).
- The `{code, data{...}}` upload response is not in the vendor docs; it comes only from the
  plugin's parser, so treat it as unverified against the vendor.
- Vendor caps `{name}` at 500 chars; the plugin does not validate filename length.
- Vendor documents `/api/locations` and the `/api/fs` file-manager endpoints (create/rename/
  move/delete, notes); the plugin uses only the PUT upload and `/api/account`.
- Upload success is decided by the presence of `data`, not by HTTP status; a 2xx or non-2xx
  with a `data` object would both be accepted.
