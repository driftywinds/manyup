# api-refs

Offline copies of the HTTP APIs that each manyup service plugin talks to.

These files are **reference material**, not build inputs. They exist so a plugin's
contract can be checked without hunting through vendor sites — some of which are
client-rendered, sign-in gated, or have gone offline.

## Services

| Service | Plugin | Auth | Upload method | Max size | Reference |
|---|---|---|---|---|---|
| 0x0.st | `0x0.go` | none | `multipart/form-data` POST | 512 MiB | [0x0.md](0x0.md) |
| BuzzHeavier | `buzzheavier.go` | optional token | raw `PUT` | unlimited | [buzzheavier.md](buzzheavier.md) |
| DataNodes | `datanodes.go` | **required** key | resolve server + multipart POST | 3 GB free / unlimited premium | [datanodes.md](datanodes.md) |
| GoFile | `gofile.go` | optional token | `multipart/form-data` POST | unlimited | [gofile.md](gofile.md) |
| Mocha | `mocha.go` | **required** key | multipart presigned `PUT` | 20 GiB (per account) | [mocha.md](mocha.md) |
| PixelDrain | `pixeldrain.go` | optional key | raw `PUT` | plan-dependent | [pixeldrain.md](pixeldrain.md) |
| VikingFile | `vikingfile.go` | optional hash | chunked presigned `PUT` | unlimited | [vikingfile.md](vikingfile.md) |
| ZincDrive | `zincdrive.go` | **required** key | S3 presigned `PUT` + confirm | 10 GB | [zincdrive.md](zincdrive.md) |

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
| `API_TOKEN` | BuzzHeavier, DataNodes, GoFile, Mocha, PixelDrain, VikingFile, ZincDrive |

The uploader merges env vars of the form `MANYUP_<SERVICE>_<KEY>` into that map, so
`MANYUP_MOCHA_API_TOKEN` is the env fallback for `manyup config set mocha API_TOKEN ...`.

### Reliability contract

Every plugin must:

- stream the file and never buffer it whole in memory;
- honour `context` cancellation;
- return errors whose message includes `status <code>` when the failure came from an HTTP
  response, because `internal/uploader/retry.go` matches that pattern to decide whether an
  attempt is retryable. `408`, `425`, `429` and `5xx` are retried; everything else fails
  fast.
