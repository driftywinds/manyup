# GoFile — API reference

> Reference for the API surface manyup uses, checked against the vendor docs saved as
> `gofile.txt` in this directory.
> Official docs: <https://gofile.io/api>.

| | |
|---|---|
| **Site** | <https://gofile.io> |
| **API base** | `https://api.gofile.io` |
| **Upload URL** | `https://upload.gofile.io/uploadfile` |
| **Public URL** | `https://gofile.io/d/{code}` |
| **Auth** | Optional — account token, sent as `Authorization: Bearer` |
| **Upload method** | `multipart/form-data` POST |
| **Max file size** | Not published by the vendor; free accounts are capped at 10,000 stored contents (`error-limits`) |

All JSON endpoints live under `https://api.gofile.io`; uploads go to a separate upload
fleet. The API is in beta, so endpoints may change.

---

## Authentication

Every request is authenticated with an account token. Three equivalent transports are
documented:

| Where | How | Applies to |
|---|---|---|
| Header | `Authorization: Bearer <token>` | every request — recommended |
| Query | `?token=<token>` | GET requests only |
| JSON body | `"token": "<token>"` | non-GET requests only |

The token authenticates as the account itself, with full access — keep it server-side.
It is optional for uploads: without any token a guest account is created on the fly and
the response carries a `guestToken` (plus the new `parentFolder`) that can be reused.
Tokens are also shown on the account's profile page.

---

## Upload

`POST https://upload.gofile.io/uploadfile` with `Content-Type: multipart/form-data`.

| Field | Type | Required | Description |
|---|---|---|---|
| `file` | file | yes | the file bytes |
| `folderId` | string | no | destination folder UUID; omit to create a new public folder at the account root |
| `token` | string | no | account token; may be sent as `Authorization: Bearer` instead; with neither, a guest account is created |

The upload host picks the closest region automatically. Regional hosts
(`upload-eu-par.gofile.io`, `upload-na-phx.gofile.io`, `upload-ap-sgp.gofile.io`, …) pin
a region; manyup always uses `upload.gofile.io`.

```bash
curl -X POST "https://upload.gofile.io/uploadfile" \
  -H "Authorization: Bearer $TOKEN" \
  -F "file=@myfile.zip" \
  -F "folderId=9f8e7d6c-5b4a-3928-1716-151413121110"
```

### Response

```json
{
  "status": "ok",
  "data": {
    "id": "d4c5e6f7-a8b9-4c0d-9e1f-2a3b4c5d6e7f",
    "type": "file",
    "name": "myfile.zip",
    "parentFolder": "9f8e7d6c-5b4a-3928-1716-151413121110",
    "parentFolderCode": "x7k2p9Qm",
    "downloadPage": "https://gofile.io/d/x7k2p9Qm",
    "code": "Qp9w8eR7",
    "size": 12345678,
    "md5": "0f8adc1149b1a2c3d4e5f60718293a4b",
    "mimetype": "application/zip",
    "createTime": 1754512800,
    "modTime": 1754512800,
    "servers": ["store-1"]
  }
}
```

Branch on `status`; only `"ok"` is success. Content ids are UUIDs; files and folders also
carry a share code — 8 characters on newly created content, 6 on legacy public folders.

### Download URL

1. `data.downloadPage` (e.g. `https://gofile.io/d/{shareCode}`)
2. `https://gofile.io/d/{data.code}`
3. `https://gofile.io/d/{data.parentFolderCode}`

---

## Errors

Every response uses one envelope; failures carry only a status string:

```json
{ "status": "ok", "data": {} }
```

```json
{ "status": "error-notPremium" }
```

Branch on `status`, not the HTTP code alone — a few endpoints historically answer HTTP
200 with an error status.

| Status | HTTP | Meaning |
|---|---|---|
| `error-token` | 401 | no token, or the token was rejected |
| `error-accountId` | 403 | the requested account id is not the token's account |
| `error-notPremium` | 401 | the endpoint requires a Premium account |
| `error-rateLimit` | 429 | rate limit exceeded — back off and retry |
| `error-limits` | 403 | account quota exceeded (storage or content count) |
| `error-notFound` | 404 | unknown content (also returned to non-owners for recycled content) |
| `error-owner` / `error-notOwner` | 401 | the content belongs to another account |
| `error-field` | 400 | a parameter failed validation; the status names it (`error-contentsId`, …) |

Rate limits are enforced per endpoint, per IP and per account; the exact values are not
published, and repeated excess can get an IP banned. Uploads past the free-account
10,000-content cap fail with `error-limits`.

---

## ⚠ Divergence from the implementation

- `VerifyCredentials` calls `GET /accounts/{API_TOKEN}`; the vendor defines `/accounts/{accountId}` (a UUID, `error-accountId` on mismatch) and `/accounts/getid` — the check URL is undocumented.
- `Description()` advertises "unlimited size"; the vendor publishes no per-file limit but caps free accounts at 10,000 stored contents, so "unlimited" is unverified.
- The vendor returns a `guestToken` for tokenless uploads; the client ignores it, so every anonymous upload creates a new guest account that is never reused.
- `Upload` never inspects the HTTP status code: a 429/5xx with a non-JSON body fails as `gofile: parsing response`, so no `status <code>` reaches the retry matcher described in `api-refs/README.md`.

---

## manyup integration notes

`internal/services/gofile.go`:

- `Upload` streams a `multipart/form-data` body through an `io.Pipe`: a goroutine writes the `file` part (`multipart.Writer.CreateFormFile`, `filepath.Base(filename)`) straight from the reader, then an optional `folderId` field taken from `cfg["folderId"]` when non-empty, closes the writer, and the request uses `mw.FormDataContentType()`. The file is never buffered whole.
- POSTs to `https://upload.gofile.io/uploadfile` via the shared `httpclient`. `Authorization: Bearer <creds["API_TOKEN"]>` is set only when that key is present and non-empty; the token is never sent as a form field or query parameter.
- Reads the response body, unmarshals `{status, data}`, and rejects anything whose `status` is not `"ok"` (`gofile: API error: <status>`) or whose `data` is missing (`gofile: empty response data`). HTTP status codes are not checked.
- Builds the result with `Filename` = the local filename passed to `Upload` (not `data.name`) and `Size` = `data.size`.
- Picks the URL as `downloadPage`, else `https://gofile.io/d/` + `code`, else `https://gofile.io/d/` + `parentFolderCode`; if all three are empty, `URL` stays empty and no error is returned.
- `RequiredCredentials()` returns `nil` (token optional), so tokenless uploads are guest uploads. `Name()` is `gofile`, `DisplayName()` is `GoFile`.
- `VerifyCredentials` trims `API_TOKEN`; empty yields `plugin.ErrCredentialRejected`, otherwise it calls `authGet` on `https://api.gofile.io/accounts/` + `url.PathEscape(token)` with the Bearer header and `authCheckOutcome` maps 2xx → ok, 401/403 → `ErrCredentialRejected`, 429/5xx → inconclusive.
- `SupportsLargeUpload()` is `true`. The package-level `gofileAPITimeout` (120 s) is declared but unreferenced; requests use the shared client's 300 s timeout.

Credential: `API_TOKEN` (optional).
