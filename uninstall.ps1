#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
.SYNOPSIS
    Uninstall script for cc-router on Windows.
.DESCRIPTION
    Removes the cc-router binary, its install directory, and the
    user PATH entry created by setup.ps1.
    Does NOT uninstall Go.
.NOTES
    Run from the project root (same directory as go.mod).
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

function Test-Command ([string]$cmd) {
    return [bool](Get-Command $cmd -ErrorAction SilentlyContinue)
}

# ---------------------------------------------------------------
# Configuration (must match setup.ps1)
# ---------------------------------------------------------------
$BinaryName = "cc-router.exe"
$InstallDir = Join-Path $env:LOCALAPPDATA "cc-router"
$BinDir = Join-Path $env:LOCALAPPDATA "bin"
$BinaryPath = Join-Path $BinDir $BinaryName

# ---------------------------------------------------------------
# 0. Pre-flight warning
# ---------------------------------------------------------------
Write-Host "`n========================================" -ForegroundColor Red
Write-Host "  cc-router Uninstall" -ForegroundColor Red
Write-Host "========================================" -ForegroundColor Red
Write-Host "`nThis will remove:" -ForegroundColor Yellow
Write-Host "  - Binary:     $BinaryPath" -ForegroundColor Yellow
Write-Host "  - Config:     $InstallDir" -ForegroundColor Yellow
Write-Host "  - PATH entry: $BinDir" -ForegroundColor Yellow
Write-Host "`nWARNING:" -ForegroundColor Red -NoNewline
Write-Host " Any downstream processes, services, or integrations that depend on" -ForegroundColor Yellow
Write-Host "  cc-router will be broken. Please stop them first or update their configuration." -ForegroundColor Yellow
Write-Host "Go will NOT be uninstalled." -ForegroundColor Green
Write-Host ""

$confirm = Read-Host "Type 'yes' to proceed with uninstallation"
if ($confirm -ne "yes") {
    Write-Host "Uninstall cancelled." -ForegroundColor Cyan
    exit 0
}

# ---------------------------------------------------------------
# 1. Remove binary
# ---------------------------------------------------------------
Write-Host "`n==> Removing binary..." -ForegroundColor Cyan
if (Test-Path $BinaryPath) {
    Remove-Item -Path $BinaryPath -Force
    Write-Host "Removed: $BinaryPath" -ForegroundColor Green
} else {
    Write-Host "Binary not found at: $BinaryPath" -ForegroundColor Yellow
}

# ---------------------------------------------------------------
# 2. Remove install directory (config.toml, logs, etc.)
# ---------------------------------------------------------------
Write-Host "`n==> Removing install directory..." -ForegroundColor Cyan
if (Test-Path $InstallDir) {
    Remove-Item -Path $InstallDir -Recurse -Force
    Write-Host "Removed: $InstallDir" -ForegroundColor Green
} else {
    Write-Host "Install directory not found: $InstallDir" -ForegroundColor Yellow
}

# ---------------------------------------------------------------
# 3. Remove from user PATH
# ---------------------------------------------------------------
Write-Host "`n==> Removing from user PATH..." -ForegroundColor Cyan
$userPath = [System.Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -match [regex]::Escape($BinDir)) {
    $parts = $userPath -split ";" | Where-Object { $_ -and ($_ -ne $BinDir) }
    $newPath = ($parts | Select-Object -Unique) -join ";"
    [System.Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
    Write-Host "Removed from user PATH: $BinDir" -ForegroundColor Green
} else {
    Write-Host "PATH entry not found: $BinDir" -ForegroundColor Yellow
}

# ---------------------------------------------------------------
# 3b. Stop and remove Windows service if present
# ---------------------------------------------------------------
Write-Host "`n==> Checking for cc-router Windows service..." -ForegroundColor Cyan
$ServiceName = "cc-router"
$existingService = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existingService) {
    Write-Host "Service found. Stopping and removing..." -ForegroundColor Yellow
    if (Test-Command "nssm") {
        nssm stop $ServiceName 2>$null
        nssm remove $ServiceName confirm
        Write-Host "Service removed via NSSM." -ForegroundColor Green
    } else {
        # Fallback to sc.exe if NSSM isn't on PATH
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
        sc.exe delete $ServiceName | Out-Null
        Write-Host "Service removed via sc.exe." -ForegroundColor Green
    }
} else {
    Write-Host "No service found, skipping." -ForegroundColor Yellow
}

# ---------------------------------------------------------------
# 4. Optional: remove empty bin directory
# ---------------------------------------------------------------
if ((Test-Path $BinDir) -and (-not (Get-ChildItem $BinDir -ErrorAction SilentlyContinue))) {
    Remove-Item -Path $BinDir -Force
    Write-Host "Removed empty bin directory: $BinDir" -ForegroundColor Green
}

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
Write-Host "`n========================================" -ForegroundColor Green
Write-Host "  Uninstall complete!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host "`nReminder: if you had any downstream consumers (e.g., editor plugins," -ForegroundColor Yellow
Write-Host "scripts, or other tools) depending on cc-router, please update them." -ForegroundColor Yellow
