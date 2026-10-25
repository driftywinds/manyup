<#
.SYNOPSIS
    Installs manyup on Windows from the latest GitHub release.

.DESCRIPTION
    Downloads the latest manyup release binary, installs it into a per-user
    folder, and adds that folder to your PATH. No Go toolchain is required.

    This installs the standard release build, so config lives in
    %APPDATA%\manyup\config.json and survives replacing the binary.

.PARAMETER InstallDir
    Where to put manyup.exe. Defaults to %LOCALAPPDATA%\Programs\manyup.

.PARAMETER Version
    Install a specific release tag (e.g. v0.2.0) instead of the latest one.

.PARAMETER Completion
    Also add tab completion to your PowerShell profile.

.PARAMETER SkipPath
    Install the binary but do not touch PATH.

.PARAMETER Force
    Overwrite an existing install without printing its version first.

.EXAMPLE
    .\scripts\install.ps1

.EXAMPLE
    .\scripts\install.ps1 -Completion

.EXAMPLE
    irm https://raw.githubusercontent.com/driftywinds/manyup/main/scripts/install.ps1 | iex
#>
[CmdletBinding()]
param(
    [string]$InstallDir = "",
    [string]$Version = "",
    [switch]$Completion,
    [switch]$SkipPath,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$repo = 'driftywinds/manyup'
$assetName = 'manyup-windows-amd64.exe'
$userAgent = 'manyup-installer'

# Windows PowerShell 5.1 still defaults to TLS 1.0, which GitHub rejects.
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {
    # PowerShell 7 ignores this; nothing to do.
}

if (-not $InstallDir) {
    $base = if ($env:LOCALAPPDATA) { $env:LOCALAPPDATA } else { Join-Path $env:USERPROFILE 'AppData\Local' }
    $InstallDir = Join-Path $base 'Programs\manyup'
}

if ($env:PROCESSOR_ARCHITECTURE -and $env:PROCESSOR_ARCHITECTURE -ne 'AMD64') {
    Write-Warning "Release builds target amd64; this machine reports $env:PROCESSOR_ARCHITECTURE. The binary may not run."
}

# ── Resolve the release ────────────────────────────────────────────────

$headers = @{
    'User-Agent' = $userAgent
    'Accept'     = 'application/vnd.github+json'
}
if ($env:GITHUB_TOKEN) {
    $headers['Authorization'] = "Bearer $env:GITHUB_TOKEN"
}

$apiUrl = if ($Version) {
    "https://api.github.com/repos/$repo/releases/tags/$Version"
} else {
    "https://api.github.com/repos/$repo/releases/latest"
}

$label = if ($Version) { $Version } else { 'latest' }
Write-Host "Looking up $repo $label..."
try {
    $release = Invoke-RestMethod -Uri $apiUrl -Headers $headers
} catch {
    throw "Could not query GitHub releases: $($_.Exception.Message). Set `$env:GITHUB_TOKEN if you are rate limited."
}

# The release publishes exactly one Windows executable per version.
$asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
if (-not $asset) {
    $names = ($release.assets | ForEach-Object { $_.name }) -join ', '
    throw "Release $($release.tag_name) has no $assetName. Available assets: $names"
}

$exePath = Join-Path $InstallDir 'manyup.exe'

if ((Test-Path $exePath) -and -not $Force) {
    $existing = & $exePath version 2>$null
    if ($existing) {
        Write-Host "Replacing existing install: $existing"
    }
}

if (Get-Process -Name manyup -ErrorAction SilentlyContinue) {
    throw "manyup is running. Close it and run this script again."
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

# ── Download ───────────────────────────────────────────────────────────

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("manyup-" + [guid]::NewGuid().ToString('N') + ".exe")
try {
    Write-Host "Downloading $($release.tag_name) ($($asset.name))..."
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $tmp -Headers @{ 'User-Agent' = $userAgent } -UseBasicParsing

    $length = (Get-Item $tmp).Length
    if ($length -lt 1MB) {
        throw "Downloaded file is only $length bytes; that is not a manyup binary."
    }

    # A Windows executable starts with the bytes "MZ".
    $stream = [System.IO.File]::OpenRead($tmp)
    try {
        $signature = New-Object byte[] 2
        $null = $stream.Read($signature, 0, 2)
    } finally {
        $stream.Dispose()
    }
    if ($signature[0] -ne 0x4D -or $signature[1] -ne 0x5A) {
        throw "Downloaded file is not a Windows executable (missing MZ header)."
    }

    Move-Item -Path $tmp -Destination $exePath -Force
} finally {
    Remove-Item $tmp -Force -ErrorAction SilentlyContinue
}

# ── PATH ───────────────────────────────────────────────────────────────

$pathChanged = $false
if (-not $SkipPath) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @($userPath -split ';' | Where-Object { $_ })

    if ($entries -notcontains $InstallDir) {
        [Environment]::SetEnvironmentVariable('Path', (@($entries) + $InstallDir) -join ';', 'User')
        $pathChanged = $true
        Write-Host "Added to user PATH: $InstallDir"
    } else {
        Write-Host "Already on user PATH: $InstallDir"
    }
}

# ── Optional tab completion ────────────────────────────────────────────

if ($Completion) {
    $line = 'if (Get-Command manyup -ErrorAction SilentlyContinue) { manyup completion powershell | Out-String | Invoke-Expression }'
    if (-not (Test-Path $PROFILE)) {
        New-Item -ItemType File -Path $PROFILE -Force | Out-Null
    }
    if (Select-String -Path $PROFILE -SimpleMatch 'manyup completion powershell' -Quiet) {
        Write-Host "Tab completion already configured in $PROFILE"
    } else {
        Add-Content -Path $PROFILE -Value $line
        Write-Host "Added tab completion to $PROFILE"
    }
}

# ── Summary ────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "Installed manyup $($release.tag_name)" -ForegroundColor Green
try {
    & $exePath where
} catch {
    Write-Warning "Installed, but running it failed: $($_.Exception.Message)"
}

Write-Host ""
if ($pathChanged -or $Completion) {
    Write-Host "Open a new terminal for the changes to take effect, then run: manyup where"
} elseif (-not $SkipPath) {
    Write-Host "Run: manyup where"
}
