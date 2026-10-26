# manyup

Blazing fast multi-service file uploader. Upload files to multiple hosting services simultaneously — in parallel or one by one — from a single CLI.

> **About this project:** manyup runs entirely offline on your device. No servers, no telemetry, no cloud. Every upload goes directly from your machine to the hosting service. The entire codebase was developed using AI — from architecture to implementation to this README.

## Features

- **Multi-service uploads** — BuzzHeavier, DataNodes, FuckingFast, GoFile, interim, Mocha, PixelDrain, storage.to, VikingFile, ZincDrive, and 0x0.st out of the box
- **Parallel or sequential** — upload to all selected services at once, or one at a time
- **Real-time progress bars** — live speed, percentage, and ETA per service using ANSI terminal rendering
- **Plugin architecture** — add new hosting services by implementing a single Go interface
- **Validated credentials** — a token is checked against the service before it is saved, so typos never make it to disk
- **Shell completion** — tab-complete service names and subcommands in bash, zsh, fish and PowerShell
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
| [FuckingFast](https://fuckingfast.net) | Optional (API_TOKEN) | Raw PUT | Unlimited |
| [GoFile](https://gofile.io) | Optional (API_TOKEN) | Multipart POST | Unlimited |
| [interim](https://interim.drifty.win) | **Required** (user hash) | Single PUT / S3 multipart | 7 GB |
| [Mocha](https://mocha.my) | **Required** (API_TOKEN) | Multipart presigned PUT | 20 GiB per file |
| [PixelDrain](https://pixeldrain.com) | **Required** (API_TOKEN) | Raw PUT | Plan-dependent |
| [storage.to](https://storage.to) | Optional (API_TOKEN) | init → presigned PUT → confirm | 25 GB free / 100 GB paid |
| [VikingFile](https://vikingfile.com) | Optional (API_TOKEN) | Streaming multipart | Unlimited |
| [ZincDrive](https://zincdrive.com) | **Required** (API_TOKEN) | S3 presigned PUT | 10 GB |

interim uploads are authenticated with a user hash; manyup always sends one. On an
instance with public uploads disabled, the hash must also be approved by the admin.
Files expire according to the instance's retention period (10 days at the time of
writing) and storage is capped at 200 GB; `/api/v1/status` is authoritative.

## Installation

### Windows — installer script

The script downloads the latest release into `%LOCALAPPDATA%\Programs\manyup`, adds that
folder to your **user** PATH, and prints the result. No Go toolchain and no admin rights
are needed.

From a clone:

```powershell
.\scripts\install.ps1
```

Or without cloning anything:

```powershell
irm https://raw.githubusercontent.com/driftywinds/manyup/main/scripts/install.ps1 | iex
```

Useful switches:

| Switch | Effect |
|---|---|
| `-Completion` | Also enable tab completion in your PowerShell profile |
| `-Version v0.2.7` | Install a specific release instead of the latest |
| `-InstallDir <path>` | Install somewhere else |
| `-SkipPath` | Install the binary but leave PATH alone |
| `-Force` | Skip the "replacing existing install" notice |

Open a **new** terminal afterwards — PATH changes never reach shells that are already open.

### Download a binary manually

Grab `manyup-<os>-<arch>` (`.exe` on Windows) from
[Releases](https://github.com/driftywinds/manyup/releases) and put it in a folder that
stays put, such as `%LOCALAPPDATA%\Programs\manyup` on Windows or `~/.local/bin` on
macOS/Linux.

Don't run it from inside a clone: `go build` overwrites the binary in the repo root.

### Add manyup to PATH

Only needed if you installed the binary manually. Adding a folder to PATH is what makes
`manyup` work from any directory; it also lets tab completion find the binary.

Windows (PowerShell) — adds to your user PATH, no admin required, and is safe to run
twice:

```powershell
$dir = "$env:LOCALAPPDATA\Programs\manyup"

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($userPath -split ';') -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
    Write-Host "Added to PATH: $dir"
}

# make it work in this window too
$env:Path += ";$dir"
```

macOS / Linux:

```bash
# add to ~/.bashrc or ~/.zshrc
export PATH="$HOME/.local/bin:$PATH"
```

Avoid `setx` on Windows: it truncates PATH at 1024 characters. Prefer
`[Environment]::SetEnvironmentVariable`, as above.

### Verify the install

```bash
manyup where
```

```
manyup 0.2.7
  binary:  C:\Users\you\AppData\Local\Programs\manyup\manyup.exe
  config:  C:\Users\you\AppData\Roaming\manyup\config.json
  on PATH: yes
```

If `on PATH` says no, the block above tells you which folder to add.

### Build from source

Requires Go 1.27+ (the version pinned in `go.mod`).

```bash
git clone https://github.com/driftywinds/manyup.git
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
| `manyup config set <service>` | Prompt for the API token (safe for any characters) |
| `manyup config set <service> <value>` | Set the API token (key defaults to `API_TOKEN`) |
| `manyup config set <service> <key> <value>` | Set any credential explicitly |
| `manyup config show` | Show current configuration |
| `manyup config mode <parallel\|sequential>` | Set upload mode |
| `manyup config select <service>` | Toggle a service on/off |
| `manyup config retry <max> [seconds]` | Set retries per service and base backoff delay |
| `manyup completion <shell>` | Print a tab-completion script |
| `manyup where` | Show the binary path, config path, and whether it is on PATH |
| `manyup version` | Print version |
| `manyup update` / `manyup -U` | Replace the current binary with the latest release |
| `manyup help` | Show usage |

### Updating

`manyup -U` downloads the latest release and replaces the running binary in place:

```
$ manyup -U
Checking for updates...
Current version: 0.1.0
Latest version:  v0.2.7
Platform:        windows-amd64

Downloading v0.2.7...
...............

✓ Updated to v0.2.7. The next time you run manyup it will be the new version.
```

The swap happens inside the install folder, so it is a rename rather than a copy. On
Windows the running image is renamed to `manyup.exe.old` (a running executable can be
renamed but not overwritten) and removed automatically on the next start. If anything goes
wrong — unwritable install folder, truncated download, a file that is not an executable —
the update fails with a clear message and the existing binary is left untouched.

Builds from source carry whatever version is compiled in. If that is not `X.Y.Z`, the
update treats it as older than any release and proceeds.

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

Only needed for services that require authentication (currently DataNodes, interim, Mocha, PixelDrain and ZincDrive):

```bash
# Omit the value to paste the token at a prompt — works with any characters
manyup config set storageto

# The credential key defaults to API_TOKEN — this is the short form
manyup config set mocha mocha_xxxxxxxxxxxxxxxxx

# DataNodes (required)
manyup config set datanodes your_api_key_here

# interim (required — user hash; approval needed when uploads are restricted)
manyup config set interim interim_xxxxxxxxxxxxxxxxx

# ZincDrive (required)
manyup config set zincdrive your_api_key_here

# GoFile (optional — works anonymous, but token ties uploads to your account)
manyup config set gofile your_gofile_token

# PixelDrain (required — uploads are rejected without a key)
manyup config set pixeldrain your_pixeldrain_api_key

# BuzzHeavier (optional — works anonymous)
manyup config set buzzheavier your_account_id

# FuckingFast (optional — works anonymous, account id ties uploads to your account)
manyup config set fuckingfast your_account_id

# VikingFile (optional — works anonymous)
manyup config set vikingfile your_user_hash

# storage.to (optional — anonymous uploads expire in 3 days; a token removes the caps)
manyup config set storageto your_api_token

# Explicit form, for any other credential key
manyup config set gofile API_TOKEN your_gofile_token
```

#### Tokens containing shell characters

A token passed as an argument is parsed by your shell first, so characters like `|`, `&`,
`<`, `>`, `^`, `(`, `)` or `%` are consumed before manyup sees them. In `cmd` this looks
like:

```
C:\> manyup config set storageto 896|UIVJ5F3m...
'UIVJ5F3m...' is not recognized as an internal or external command
```

Nothing is saved in that case — manyup only ever received `896`, and the verification step
rejects it. Two ways to pass such a token safely:

```bash
# 1. quote it for your shell
manyup config set storageto "896|UIVJ5F3mMOmetvdGT7BZat8PGCmtXeYDU5xJqgS7"

# 2. omit the value and paste it at the prompt (keeps it out of shell history)
manyup config set storageto
# Paste the API_TOKEN for storage.to and press Enter:
```

Reading from stdin is also scriptable, and works for tokens with any characters:

```bash
manyup config set storageto < token.txt
cat token.txt | manyup config set storageto -
```

The prompt echoes what you type (manyup has no terminal-control dependency), so use the
redirect form if someone is looking over your shoulder.

**Credentials are verified before they are saved.** `config set` calls the service's own
account endpoint with the token it was given, and a token the service rejects is never
written to disk:

```
$ manyup config set mocha mocha_typo
✗ mocha: credential rejected: {"error":"Unauthorized"}
Credential was not saved.

$ manyup config set mocha mocha_correct
✓ Set mocha/API_TOKEN (verified)
```

Services with a known check: Mocha, GoFile, DataNodes, interim, ZincDrive, BuzzHeavier, PixelDrain.
For interim the check also consults `/api/v1/status`: an unapproved hash is rejected when
public uploads are disabled and accepted when they are enabled.
If the service cannot be reached the token is saved with a warning instead, so a temporary
outage never blocks configuration; VikingFile has no account endpoint, so its token is
always saved unverified.

### Tab completion

Completion is generated by the binary, so it always matches the registered services. Add
one line to your shell profile and restart the shell:

```bash
# bash (~/.bashrc)
eval "$(manyup completion bash)"

# zsh (~/.zshrc)
eval "$(manyup completion zsh)"

# fish (~/.config/fish/config.fish)
manyup completion fish | source
```

```powershell
# PowerShell — add to $PROFILE
manyup completion powershell | Out-String | Invoke-Expression
```

Then `manyup config set <TAB>` cycles through every service, and `manyup config <TAB>`
lists the subcommands:

```
$ manyup config set <TAB>
0x0  buzzheavier  datanodes  gofile  interim  mocha  pixeldrain  vikingfile  zincdrive
```

#### cmd.exe

`cmd.exe` has no completion mechanism of its own — not even for filenames — so manyup
cannot hook into it directly. The only way to get tab completion inside `cmd` is
[Clink](https://github.com/chrisant996/clink), which replaces the cmd prompt with a
readline-style editor:

```cmd
winget install --id chrisant996.Clink -e
```

Then install the completion script:

```cmd
if not exist "%LOCALAPPDATA%\clink" mkdir "%LOCALAPPDATA%\clink"
manyup completion clink > "%LOCALAPPDATA%\clink\manyup.lua"
```

The script calls `manyup` by name, so **manyup must be on your PATH** — otherwise Tab
silently falls back to file completion. Confirm with `where manyup`.

Open a new `cmd` window, or press <kbd>Ctrl</kbd>+<kbd>X</kbd> <kbd>Ctrl</kbd>+<kbd>R</kbd>
to reload scripts. `clink info` prints the version, profile directory and the script
directories Clink actually reads; if `clink info` itself is not recognised, Clink is not
injected into that `cmd` window (run `clink autorun install`).

Alternatively, skip `cmd` entirely: the bash script works in **Git Bash** (which ships with
Git for Windows), and the PowerShell script works in Windows Terminal.

> Tab completion is shell-side by design: `manyup completion <shell>` prints a script, and
> nothing happens until that script is loaded into your shell. A program cannot register a
> handler inside the shell that launched it.

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
export MANYUP_INTERIM_API_TOKEN=interim_xxxxxxxxxxxxxxxxx
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
├── scripts/
│   └── install.ps1                  # Windows installer (latest release + PATH)
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
│       ├── verify.go                # Shared credential-check helpers
│       ├── buzzheavier.go           # BuzzHeavier plugin (PUT upload)
│       ├── datanodes.go             # DataNodes plugin (two-step multipart API)
│       ├── fuckingfast.go           # FuckingFast plugin (PUT upload)
│       ├── gofile.go                # GoFile plugin (multipart upload)
│       ├── interim.go               # interim plugin (single PUT or S3 multipart)
│       ├── mocha.go                 # Mocha plugin (multipart presigned + share link)
│       ├── 0x0.go                   # 0x0.st plugin (multipart POST upload)
│       ├── pixeldrain.go            # PixelDrain plugin (PUT upload)
│       ├── storageto.go             # storage.to plugin (presigned PUT, multipart)
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
4. Optionally implement `plugin.CredentialVerifier` so `manyup config set` can validate
   the token against the service before saving it

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

manyup is licensed under the [GNU Affero General Public License v3.0](LICENSE)
(AGPL-3.0). See [LICENSE](LICENSE) for the full text.

AGPL is a strong copyleft license. In addition to the usual distribution terms, §13
requires that anyone who runs a modified version as a network service offers the
corresponding source to that service's users.
