#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
.SYNOPSIS
    One-shot setup script for cc-router on Windows.
.DESCRIPTION
    - Downloads the latest cc-router release binary from GitHub
    - Installs it to the user's local bin directory
    - Ensures the bin directory is on the user PATH
    - Creates a sample config.toml if missing
.NOTES
    No Go installation required.
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# ---------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------
$Repo = "blobbyblo/ClaudeCodeRouter"
$AssetName = "cc-router_Windows_x86_64.zip"
$BinaryName = "cc-router.exe"
$InstallDir = Join-Path $env:LOCALAPPDATA "cc-router"
$BinDir = Join-Path $env:LOCALAPPDATA "bin"
$BinaryPath = Join-Path $BinDir $BinaryName

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
# 2. Download latest release binary
# ---------------------------------------------------------------
Write-Host "`n==> Downloading cc-router..." -ForegroundColor Cyan

$TempZip = Join-Path $env:TEMP "cc-router-release.zip"
$TempExtract = Join-Path $env:TEMP "cc-router-extract"

Write-Host "Fetching latest release info..."
$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" `
    -Headers @{ "User-Agent" = "cc-router-setup" }
$asset = $release.assets | Where-Object { $_.name -eq $AssetName }
if (-not $asset) {
    throw "Could not find release asset '$AssetName' in latest release $($release.tag_name)."
}

Write-Host "Downloading $AssetName (release $($release.tag_name))..."
Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $TempZip

Write-Host "Extracting..."
if (Test-Path $TempExtract) { Remove-Item $TempExtract -Recurse -Force }
Expand-Archive -Path $TempZip -DestinationPath $TempExtract -Force
Remove-Item $TempZip -ErrorAction SilentlyContinue

$extractedBinary = Get-ChildItem -Path $TempExtract -Recurse -Filter $BinaryName |
    Select-Object -First 1
if (-not $extractedBinary) {
    throw "Binary '$BinaryName' not found after extraction."
}
Write-Host "Download successful." -ForegroundColor Green

# ---------------------------------------------------------------
# 3. Install to user bin
# ---------------------------------------------------------------
Write-Host "`n==> Installing binary..." -ForegroundColor Cyan
Ensure-Dir $BinDir
Copy-Item -Path $extractedBinary.FullName -Destination $BinaryPath -Force
Remove-Item $TempExtract -Recurse -Force -ErrorAction SilentlyContinue
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

# ---------------------------------------------------------------
# 7. Optional: Install as Windows service via NSSM
# ---------------------------------------------------------------
Write-Host ""
$installService = Read-Host "Install cc-router as a Windows service? (y/N)"
if ($installService -notmatch '^[Yy]$') {
    Write-Host "Skipping service installation." -ForegroundColor Yellow
    exit 0
}

# Ensure winget is available
Write-Host "`n==> Checking for winget..." -ForegroundColor Cyan
if (-not (Test-Command "winget")) {
    Write-Host "winget not found. Please install App Installer from the Microsoft Store and re-run." -ForegroundColor Red
    exit 1
}
Write-Host "winget is available." -ForegroundColor Green

# Ensure NSSM is installed
Write-Host "`n==> Checking for NSSM..." -ForegroundColor Cyan
if (-not (Test-Command "nssm")) {
    Write-Host "Installing NSSM via winget..."
    winget install nssm --silent --accept-package-agreements --accept-source-agreements
    # Refresh PATH for this session
    $env:PATH = [System.Environment]::GetEnvironmentVariable("PATH", "Machine") + ";" +
                [System.Environment]::GetEnvironmentVariable("PATH", "User")
    if (-not (Test-Command "nssm")) {
        Write-Host "NSSM installed but not yet on PATH. Please restart your terminal and re-run setup." -ForegroundColor Yellow
        exit 1
    }
}
Write-Host "NSSM is available." -ForegroundColor Green

# Stop and remove existing service if present
$ServiceName = "cc-router"
$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existingService) {
    Write-Host "Existing service found. Removing..." -ForegroundColor Yellow
    nssm stop $ServiceName 2>$null
    nssm remove $ServiceName confirm
    Write-Host "Existing service removed." -ForegroundColor Green
}

# Create log directory
$LogDir = Join-Path $InstallDir "logs"
Ensure-Dir $LogDir
Write-Host "Log directory: $LogDir" -ForegroundColor Green

# Register the service
Write-Host "`n==> Registering cc-router as a service..." -ForegroundColor Cyan
nssm install $ServiceName $BinaryPath
nssm set $ServiceName AppParameters "-config `"$ConfigPath`""
nssm set $ServiceName AppDirectory $InstallDir
nssm set $ServiceName DisplayName "Claude Code Router"
nssm set $ServiceName Description "CCR proxy router for Claude Code"
nssm set $ServiceName Start SERVICE_AUTO_START
nssm set $ServiceName AppStdout (Join-Path $LogDir "stdout.log")
nssm set $ServiceName AppStderr (Join-Path $LogDir "stderr.log")
nssm set $ServiceName AppRotateFiles 1
nssm set $ServiceName AppRotateBytes 10485760

# Start the service
Write-Host "`n==> Starting service..." -ForegroundColor Cyan
nssm start $ServiceName

# Verify it started
Start-Sleep -Seconds 2
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq "Running") {
    Write-Host "Service is running." -ForegroundColor Green
} else {
    Write-Host "Service may have failed to start. Check logs:" -ForegroundColor Red
    Write-Host "  $LogDir\stderr.log"
    exit 1
}

# Read admin port from config for the URL
$AdminPort = 4081
$adminPortMatch = Select-String -Path $ConfigPath -Pattern "admin_port\s*=\s*(\d+)"
if ($adminPortMatch) {
    $AdminPort = $adminPortMatch.Matches[0].Groups[1].Value
}

Write-Host ""
Write-Host "========================================" -ForegroundColor Green
Write-Host "  Service installed and running!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host ""
Write-Host "  Service name:  $ServiceName"
Write-Host "  Logs:          $LogDir"
Write-Host ""

$openAdmin = Read-Host "Open the admin panel in your browser? (y/N)"
if ($openAdmin -match '^[Yy]$') {
    Start-Process "http://127.0.0.1:$AdminPort"
}
