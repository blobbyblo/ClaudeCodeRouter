#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
.SYNOPSIS
    One-shot setup script for cc-router on Windows.
.DESCRIPTION
    - Installs Go if missing
    - Builds the cc-router binary
    - Installs it to the user's local bin directory
    - Ensures the bin directory is on the user PATH
    - Creates a sample config.toml if missing
.NOTES
    Run from the project root (the same directory as go.mod).
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

# ---------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------
$ProjectRoot = $PSScriptRoot
$GoVersion = "1.23.4"
$BinaryName = "cc-router.exe"
$InstallDir = Join-Path $env:LOCALAPPDATA "cc-router"
$BinDir = Join-Path $env:LOCALAPPDATA "bin"
$BinaryPath = Join-Path $BinDir $BinaryName
$GoDownloadUrl = "https://go.dev/dl/go${GoVersion}.windows-amd64.zip"
$TempDir = Join-Path $env:TEMP "cc-router-setup"

# ---------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------
function Test-Command ([string]$cmd) {
    return [bool](Get-Command $cmd -ErrorAction SilentlyContinue)
}

function Add-ToUserPath ([string]$dir) {
    $current = [System.Environment]::GetEnvironmentVariable("PATH", "User")
    $parts = $current -split ";" | Where-Object { $_ -and (Test-Path $_) -and ($_ -ne $dir) }
    $parts = @($dir) + $parts
    $newPath = ($parts | Select-Object -Unique) -join ";"
    [System.Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
    Write-Host "Added to user PATH: $dir"
}

function Ensure-Dir ([string]$dir) {
    if (-not (Test-Path $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
}

# ---------------------------------------------------------------
# 1. Ensure Go is installed
# ---------------------------------------------------------------
function Install-Go {
    Write-Host "`n==> Go not found. Installing Go $GoVersion..." -ForegroundColor Cyan

    Ensure-Dir $TempDir
    $goZip = Join-Path $TempDir "go.zip"
    $goInstallDir = Join-Path $env:LOCALAPPDATA "go"

    Write-Host "Downloading Go $GoVersion..."
    Invoke-WebRequest -Uri $GoDownloadUrl -OutFile $goZip -ProgressPreference Continue

    Write-Host "Extracting Go..."
    Expand-Archive -Path $goZip -DestinationPath $TempDir -Force

    # Remove old go installation if it exists
    if (Test-Path $goInstallDir) {
        Remove-Item -Recurse -Force $goInstallDir
    }
    Move-Item -Path (Join-Path $TempDir "go") -Destination $goInstallDir -Force

    # Add Go to user PATH
    $goBin = Join-Path $goInstallDir "bin"
    if (-not (Test-Command "go")) {
        Add-ToUserPath $goBin
    }

    # Refresh PATH for this session
    $env:PATH = "$goBin;$env:PATH"
    Remove-Item $goZip -ErrorAction SilentlyContinue

    # Verify
    if (Test-Command "go") {
        Write-Host "Go installed: $(go version)" -ForegroundColor Green
    } else {
        throw "Go installation failed."
    }
}

Write-Host "`n==> Checking for Go..." -ForegroundColor Cyan
if (-not (Test-Command "go")) {
    Install-Go
} else {
    Write-Host "Go is already installed: $(go version)" -ForegroundColor Green
}

# ---------------------------------------------------------------
# 2. Download dependencies & build
# ---------------------------------------------------------------
Write-Host "`n==> Building cc-router..." -ForegroundColor Cyan
Set-Location $ProjectRoot

Write-Host "Downloading Go modules..."
go mod download

Write-Host "Compiling binary..."
go build -ldflags="-s -w" -o (Join-Path $ProjectRoot "cc-router.exe") ./cmd/ccr

if (-not (Test-Path (Join-Path $ProjectRoot "cc-router.exe"))) {
    throw "Build failed. Binary not found."
}
Write-Host "Build successful." -ForegroundColor Green

# ---------------------------------------------------------------
# 3. Install to user bin
# ---------------------------------------------------------------
Write-Host "`n==> Installing binary..." -ForegroundColor Cyan
Ensure-Dir $BinDir
Copy-Item -Path (Join-Path $ProjectRoot "cc-router.exe") -Destination $BinaryPath -Force
Write-Host "Installed to: $BinaryPath" -ForegroundColor Green

$env:PATH = "$BinDir;$env:PATH"

# ---------------------------------------------------------------
# 4. Ensure bin directory is in user PATH
# ---------------------------------------------------------------
Write-Host "`n==> Checking PATH..." -ForegroundColor Cyan
$userPath = [System.Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -notmatch [regex]::Escape($BinDir)) {
    Add-ToUserPath $BinDir
    Write-Host "PATH updated. You may need to restart your terminal." -ForegroundColor Yellow
} else {
    Write-Host "PATH already includes $BinDir" -ForegroundColor Green
}

# ---------------------------------------------------------------
# 5. Ensure install directory and sample config exist
# ---------------------------------------------------------------
Write-Host "`n==> Checking install directory..." -ForegroundColor Cyan
Ensure-Dir $InstallDir

$ConfigPath = Join-Path $InstallDir "config.toml"
if (-not (Test-Path $ConfigPath)) {
    Write-Host "Creating sample config.toml..."
    @"
[server]
host = "127.0.0.1"
client_port = 8080
admin_port = 8081
log_level = "info"

# Add providers as needed
# [[provider]]
# name = "anthropic"
# api_key = "your-api-key-here"
# enabled = true
"@ | Set-Content -Path $ConfigPath -Encoding UTF8
    Write-Host "Created sample config: $ConfigPath" -ForegroundColor Green
} else {
    Write-Host "Config already exists: $ConfigPath" -ForegroundColor Green
}

# ---------------------------------------------------------------
# 6. Verify
# ---------------------------------------------------------------
Write-Host "`n==> Verifying installation..." -ForegroundColor Cyan
$resolved = Get-Command "cc-router" -ErrorAction SilentlyContinue
if ($resolved) {
    Write-Host "cc-router is available on the PATH at: $($resolved.Source)" -ForegroundColor Green
} else {
    Write-Host "cc-router is not yet in PATH. Restart your terminal and run 'cc-router -version' to verify." -ForegroundColor Yellow
}

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
Write-Host "`n========================================" -ForegroundColor Green
Write-Host "  Setup complete!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host ""
Write-Host "  Binary:     $BinaryPath"
Write-Host "  Config:     $ConfigPath"
Write-Host "  Data dir:   $InstallDir"
Write-Host ""
Write-Host "Usage:" -ForegroundColor Cyan
Write-Host "  cc-router -config `"$ConfigPath`"`n"
