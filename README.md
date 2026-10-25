# manyup

Blazing fast multi-service file uploader. Upload files to multiple hosting services simultaneously — in parallel or one by one — from a single CLI.

> **About this project:** manyup runs entirely offline on your device. No servers, no telemetry, no cloud. Every upload goes directly from your machine to the hosting service. The entire codebase was developed using AI — from architecture to implementation to this README.

## Features

- **Multi-service uploads** — BuzzHeavier, DataNodes, GoFile, Mocha, PixelDrain, VikingFile, ZincDrive, and 0x0.st out of the box
- **Parallel or sequential** — upload to all selected services at once, or one at a time
- **Real-time progress bars** — live speed, percentage, and ETA per service using ANSI terminal rendering
- **Plugin architecture** — add new hosting services by implementing a single Go interface
- **Streaming uploads** — files are streamed, never buffered entirely in memory. Uploads multi-gigabyte files with minimal RAM usage
- **Automatic retries** — transient failures (dropped connections, timeouts, rate limits, 5xx responses) are retried per service with exponential backoff, while permanent errors fail fast
- **Resilient** — missing credentials skip a service with a warning instead of blocking everything
- **Cross-platform** — builds for Windows, Linux, and macOS (amd64 + arm64)
- **Zero dependencies** — single static binary, no runtime requirements

## Supported Services

| Service | Auth | Upload Method | Max Size |
|---|---|---|---|
| [0x0.st](https://0x0.st) | None | Multipart POST | 512 MB |
| [BuzzHeavier](https://buzzheavier.com) | Optional (API_TOKEN) | Raw PUT | Unlimited |
| [DataNodes](https://datanodes.to) | **Required** (API_TOKEN) | Two-step multipart POST | 3 GB free / unlimited premium |
| [GoFile](https://gofile.io) | Optional (API_TOKEN) | Multipart POST | Unlimited |
| [Mocha](https://mocha.my) | **Required** (API_TOKEN) | Multipart presigned PUT | 20 GiB per file |
| [PixelDrain](https://pixeldrain.com) | Optional (API_TOKEN) | Raw PUT | Plan-dependent |
| [VikingFile](https://vikingfile.com) | Optional (API_TOKEN) | Streaming multipart | Unlimited |
| [ZincDrive](https://zincdrive.com) | **Required** (API_TOKEN) | S3 presigned PUT | 10 GB |

## Installation

### Download

Grab the latest binary for your platform from [Releases](https://github.com/multiuploader/manyup/releases).

### Build from source

Requires [Go 1.21+](https://go.dev/dl/).

```bash
git clone https://github.com/multiuploader/manyup.git
cd manyup
go build -o manyup .
```

## Usage

```
manyup <command> [options]
```

### Commands

| Command | Description |
|---|---|
| `manyup upload <file> [file...]` | Upload file(s) to selected services |
| `manyup services` | List available service plugins |
| `manyup config set <service> <key> <value>` | Set a credential for a service |
| `manyup config show` | Show current configuration |
| `manyup config mode <parallel\|sequential>` | Set upload mode |
| `manyup config select <service>` | Toggle a service on/off |
| `manyup config retry <max> [seconds]` | Set retries per service and base backoff delay |
| `manyup version` | Print version |
| `manyup update` / `manyup -U` | Check for and apply updates |
| `manyup help` | Show usage |

### Quick start

```bash
# 1. Select which services to upload to
manyup config select gofile
manyup config select buzzheavier

# 2. Set upload mode (parallel by default)
manyup config mode parallel

# 3. Upload!
manyup upload myfile.zip
```

### Configure credentials

Only needed for services that require authentication (currently DataNodes, Mocha and ZincDrive):

```bash
# DataNodes (required)
manyup config set datanodes API_TOKEN your_api_key_here

# Mocha (required — create a key at https://mocha.my/api-keys)
manyup config set mocha API_TOKEN mocha_xxxxxxxxxxxxxxxxx

# ZincDrive (required)
manyup config set zincdrive API_TOKEN your_api_key_here

# GoFile (optional — works anonymous, but token ties uploads to your account)
manyup config set gofile API_TOKEN your_gofile_token

# PixelDrain (optional — anonymous works, but token increases limits)
manyup config set pixeldrain API_TOKEN your_pixeldrain_api_key

# BuzzHeavier (optional — works anonymous)
manyup config set buzzheavier API_TOKEN your_account_id

# VikingFile (optional — works anonymous)
manyup config set vikingfile API_TOKEN your_user_hash
```

### Upload multiple files

```bash
manyup upload *.zip video.mp4 document.pdf
```

### What you'll see

```
⬆  Uploading: myfile.zip
  gofile       ████████████████████  100.0%  12.3 MB/s  ETA -
  buzzheavier  ████████████████████  100.0%   8.1 MB/s  ETA -
────────────────────────────────────────────────────────────
  ✓ gofile: https://gofile.io/d/abc123 (2.1s)
  ✓ buzzheavier: https://buzzheavier.com/xyz789 (3.4s)
  Total time: 3.4s
────────────────────────────────────────────────────────────
```

## Configuration

Config is stored in a JSON file at:

| Platform | Path |
|---|---|
| Windows | `%APPDATA%/manyup/config.json` |
| Linux | `~/.config/manyup/config.json` |
| macOS | `~/.config/manyup/config.json` |

### Environment variables

Credentials can also be set via environment variables instead of the config file:

```bash
export MANYUP_DATANODES_API_TOKEN=your_key
export MANYUP_MOCHA_API_TOKEN=mocha_xxxxxxxxxxxxxxxxx
export MANYUP_ZINCDRIVE_API_TOKEN=your_key
export MANYUP_GOFILE_API_TOKEN=your_token
export MANYUP_PIXELDRAIN_API_TOKEN=your_token
manyup upload myfile.zip
```

Format: `MANYUP_<SERVICE>_API_TOKEN`

### Upload retries

When a service upload fails with a transient error — dropped/reset connections, timeouts, unexpected EOFs, rate limiting (429), or server-side errors (5xx) — manyup automatically retries that service with exponential backoff (jittered so parallel services don't reconnect in lockstep). Permanent failures such as bad requests, auth errors, or 403/404 responses fail immediately without wasting time.

```bash
# 3 total attempts per service (default: 2 retries, 2s base delay)
manyup config retry 2

# 5 retries with a 5s base delay: waits ~5s, 10s, 20s, 30s, 30s
manyup config retry 5 5

# Disable retries entirely
manyup config retry 0
```

Each wait doubles per attempt and is capped at 30 seconds. Retries can also be set in `config.json` via `max_retries` and `retry_delay_seconds`.

## Architecture

```
manyup/
├── main.go                          # CLI entry point + progress display
├── api-refs/                        # Offline HTTP API references for every service
├── internal/
│   ├── httpclient/
│   │   └── httpclient.go            # Shared tuned HTTP client (256KB buffers, connection pooling)
│   ├── plugin/
│   │   └── plugin.go                # Uploader interface + Registry
│   ├── config/
│   │   └── config.go                # JSON config, API keys, service selection
│   ├── uploader/
│   │   ├── uploader.go              # Upload orchestration (parallel/sequential)
│   │   ├── progress_reader.go       # Streaming progress tracking with smoothed speed
│   │   └── retry.go                 # Transient-failure classification + backoff
│   └── services/
│       ├── registry.go              # Central service registration
│       ├── buzzheavier.go           # BuzzHeavier plugin (PUT upload)
│       ├── datanodes.go             # DataNodes plugin (two-step multipart API)
│       ├── gofile.go                # GoFile plugin (multipart upload)
│       ├── mocha.go                 # Mocha plugin (multipart presigned + share link)
│       ├── 0x0.go                   # 0x0.st plugin (multipart POST upload)
│       ├── pixeldrain.go            # PixelDrain plugin (PUT upload)
│       ├── vikingfile.go            # VikingFile plugin (legacy + chunked fallback)
│       └── zincdrive.go             # ZincDrive plugin (S3 presigned upload)
```

Each plugin's HTTP contract is documented in [`api-refs/`](api-refs/README.md) — one file
per service, covering auth, endpoints, request/response bodies, error codes and how the
plugin streams the file.

### Adding a new service

1. Create `internal/services/yourservice.go`
2. Implement the `plugin.Uploader` interface (6 methods)
3. Register it in `internal/services/registry.go`

```go
type Uploader interface {
    Name() string
    DisplayName() string
    Description() string
    RequiredCredentials() []string
    Upload(ctx context.Context, filename string, reader io.Reader, size int64, creds Credentials, cfg Config) (*UploadResult, error)
    SupportsLargeUpload() bool
}
```

## Development

```bash
# Build
go build -o manyup .

# Vet
go vet ./...

# Run directly
go run . upload myfile.zip
go run . services
go run . config show
```

## License

MIT
