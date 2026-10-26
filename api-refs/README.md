# api-refs

Offline copies of the HTTP APIs that each manyup service plugin talks to.

These files are **reference material**, not build inputs. They exist so a plugin's
contract can be checked without hunting through vendor sites — some of which are
client-rendered, sign-in gated, or have gone offline.

## Services

| Service | Plugin | Auth | Upload method | Max size | Reference |
|---|---|---|---|---|---|
| 0x0.st | `0x0.go` | none | `multipart/form-data` POST | 512 MiB | [0x0.md](0x0.md) |
| BuzzHeavier | `buzzheavier.go` | optional account id | raw `PUT` | no documented limit | [buzzheavier.md](buzzheavier.md) |
| DataNodes | `datanodes.go` | **required** key | resolve server + multipart POST | 3 GB free / unlimited premium | [datanodes.md](datanodes.md) |
| FuckingFast | `fuckingfast.go` | optional account id | raw `PUT` | no documented limit | [fuckingfast.md](fuckingfast.md) |
| GoFile | `gofile.go` | optional token | `multipart/form-data` POST | no per-file cap; free tier allows 10,000 stored files | [gofile.md](gofile.md) |
| interim | `interim.go` | **required** approved user hash | 3-step presigned `PUT` | 5 GiB single PUT | [interim.md](interim.md) |
| Mocha | `mocha.go` | **required** key | multipart presigned `PUT` | 20 GiB (per account) | [mocha.md](mocha.md) |
| PixelDrain | `pixeldrain.go` | **required** key | raw `PUT` | plan-dependent | [pixeldrain.md](pixeldrain.md) |
| storage.to | `storageto.go` | optional token | init → presigned `PUT` → confirm | 25 GB anonymous/free; 100 GB paid | [storageto.md](storageto.md) |
| VikingFile | `vikingfile.go` | optional hash | chunked presigned `PUT` | unlimited | [vikingfile.md](vikingfile.md) |
| ZincDrive | `zincdrive.go` | **required** key | S3 presigned `PUT` + confirm | 4 GB single-part; 10 GB multipart | [zincdrive.md](zincdrive.md) |

Vendor documentation snapshots live beside the references as `<service>.txt`. Each
`<service>.md` is the normalised reference derived from that snapshot plus the plugin
source, and ends with a divergence section wherever the implementation and the vendor
disagree.

`mocha.txt` and `Mocha.htm` in this directory are raw saves of the Mocha API docs page.
Both are **client-rendered shells** — the page body is empty until JavaScript runs, and
the live page requires a signed-in session. They contain no usable API description;
see [mocha.md](mocha.md) instead.

## Conventions used in these files

Each reference documents, in this order:

1. **Endpoint table** — base URLs, auth, transport, limits.
2. **Authentication** — header shape and where the credential comes from.
3. **Upload flow** — the exact requests manyup makes, with request and response bodies.
4. **Errors** — the status codes and envelopes the client has to handle.
5. **manyup integration notes** — which file implements it, how bytes are streamed, and
   which credential key it reads.

### Credential keys

Plugins read credentials from a `plugin.Credentials` map. The keys actually used are:

| Key | Services |
|---|---|
| `API_TOKEN` | BuzzHeavier, DataNodes, GoFile, interim, Mocha, PixelDrain, VikingFile, ZincDrive |

The uploader merges env vars of the form `MANYUP_<SERVICE>_<KEY>` into that map, so
`MANYUP_MOCHA_API_TOKEN` is the env fallback for `manyup config set mocha API_TOKEN ...`.

`manyup config set` accepts the short form `config set <service> <value>`, which fills
`API_TOKEN` for you.

### Credential checks

A plugin may implement `plugin.CredentialVerifier` so `manyup config set` can reject a
mistyped token before it reaches `config.json`. Each check is one cheap authenticated
request; a `401`/`403` means "rejected", while `429`/`5xx` or a transport failure is
treated as inconclusive so a working token is never discarded because a host is down.

| Service | Check request | Rejected when |
|---|---|---|
| Mocha | `GET https://api.mocha.my/api/profile/summary` | `401` |
| GoFile | `GET https://api.gofile.io/accounts/getid` | `401` (`{"status":"error-token"}`) |
| interim | `GET https://interim.drifty.win/api/v1/keys/me` | `401`/`403`, or `200` with `"approved": false` |
| DataNodes | `GET https://datanodes.to/api/upload/server?key=...` | body has no `sess_id` (HTTP stays `200`) |
| ZincDrive | `POST https://zincdrive.com/api/v1/s3/sign` | `401` |
| BuzzHeavier | `GET https://buzzheavier.com/api/account` | `401` |
| PixelDrain | `GET https://pixeldrain.com/api/user` (Basic) | `401` |
| VikingFile | *(none available)* | — saved unverified |

### Reliability contract

Every plugin must:

- stream the file and never buffer it whole in memory;
- honour `context` cancellation;
- return errors whose message includes `status <code>` when the failure came from an HTTP
  response, because `internal/uploader/retry.go` matches that pattern to decide whether an
  attempt is retryable. `408`, `425`, `429` and `5xx` are retried; everything else fails
  fast.
