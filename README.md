# manyup

Blazing fast multi-service file uploader. Upload files to multiple hosting services simultaneously — in parallel or one by one — from a single CLI.

> **About this project:** manyup runs entirely offline on your device. No servers, no telemetry, no cloud. Every upload goes directly from your machine to the hosting service. The entire codebase was developed using AI — from architecture to implementation to this README.

## Features

- **Multi-service uploads** — BuzzHeavier, DataNodes, GoFile, VikingFile, and ZincDrive out of the box
- **Parallel or sequential** — upload to all selected services at once, or one at a time
- **Real-time progress bars** — live speed, percentage, and ETA per service using ANSI terminal rendering
- **Plugin architecture** — add new hosting services by implementing a single Go interface
- **Streaming uploads** — files are streamed, never buffered entirely in memory. Uploads multi-gigabyte files with minimal RAM usage
- **Resilient** — missing credentials skip a service with a warning instead of blocking everything
- **Cross-platform** — builds for Windows, Linux, and macOS (amd64 + arm64)
- **Zero dependencies** — single static binary, no runtime requirements

## Supported Services

| Service | Auth | Upload Method | Max Size |
|---|---|---|---|
| [BuzzHeavier](https://buzzheavier.com) | Optional (API_TOKEN) | Raw PUT | Unlimited |
| [DataNodes](https://datanodes.to) | **Required** (API_TOKEN) | Two-step multipart POST | 3 GB free / unlimited premium |
| [GoFile](https://gofile.io) | Optional (API_TOKEN) | Multipart POST | Unlimited |
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

Only needed for services that require authentication (currently DataNodes and ZincDrive):

```bash
# DataNodes (required)
manyup config set datanodes API_TOKEN your_api_key_here

# ZincDrive (required)
manyup config set zincdrive API_TOKEN your_api_key_here

# GoFile (optional — works anonymous, but token ties uploads to your account)
manyup config set gofile API_TOKEN your_gofile_token

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
export MANYUP_ZINCDRIVE_API_TOKEN=your_key
export MANYUP_GOFILE_API_TOKEN=your_token
manyup upload myfile.zip
```

Format: `MANYUP_<SERVICE>_API_TOKEN`

## Architecture

```
manyup/
├── main.go                          # CLI entry point + progress display
├── internal/
│   ├── httpclient/
│   │   └── httpclient.go            # Shared tuned HTTP client (256KB buffers, connection pooling)
│   ├── plugin/
│   │   └── plugin.go                # Uploader interface + Registry
│   ├── config/
│   │   └── config.go                # JSON config, API keys, service selection
│   ├── uploader/
│   │   ├── uploader.go              # Upload orchestrator (parallel/sequential)
│   │   └── progress_reader.go       # Streaming progress tracking with smoothed speed
│   └── services/
│       ├── registry.go              # Central service registration
│       ├── buzzheavier.go           # BuzzHeavier plugin (PUT upload)
│       ├── datanodes.go             # DataNodes plugin (two-step multipart API)
│       ├── gofile.go                # GoFile plugin (multipart upload)
│       ├── vikingfile.go            # VikingFile plugin (legacy + chunked fallback)
│       └── zincdrive.go             # ZincDrive plugin (S3 presigned upload)
```

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
