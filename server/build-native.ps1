<#
.SYNOPSIS
    Build the lancedb-go native library from Rust source (Windows / MSYS2 UCRT64).

.DESCRIPTION
    Compiles the Rust crate from the lancedb-go repo
    (https://github.com/sysadminmike/lancedb-go) into a static
    library (liblancedb_go.a) compatible with the MinGW/GNU toolchain.
    The repo is cloned to server/_lancedb-go/ on first run.

    The resulting .a file is copied into server/lib/windows_amd64/ so the Go
    server can link against it via CGo.

    IMPORTANT: You almost certainly do NOT need to run this script.  Pre-built
    native libraries are checked into server/lib/ (or downloadable from the
    project releases).  Only rebuild if you are modifying the Rust or C FFI
    layer, or if the pre-built library is missing/corrupt.

.NOTES
    Prerequisites (all installed via MSYS2 UCRT64 terminal):
        pacman -S mingw-w64-ucrt-x86_64-gcc
        pacman -S mingw-w64-ucrt-x86_64-cmake
        pacman -S mingw-w64-ucrt-x86_64-nasm
        pacman -S mingw-w64-ucrt-x86_64-make
        pacman -S mingw-w64-ucrt-x86_64-protobuf
        pacman -S mingw-w64-ucrt-x86_64-rust   # OR install Rust via rustup

    Why CARGO_TARGET_DIR is set to a short path:
        The aws-lc-sys crate generates very deep directory trees during build.
        On Windows the default target/ path (inside the project tree) often
        exceeds MAX_PATH (260 chars) and causes cryptic build failures.
        Using C:\ct keeps all paths short.

    Build time: ~20 minutes on a modern machine (first build).  Subsequent
    incremental builds are much faster.
#>

param(
    [switch]$Clean
)

$ErrorActionPreference = "Stop"

# Ensure MSYS2 UCRT64 tools are on PATH
$msysBin = "C:\msys64\ucrt64\bin"
if (Test-Path $msysBin) {
    $env:PATH = "$msysBin;" + $env:PATH
} else {
    Write-Host "ERROR: MSYS2 UCRT64 not found at $msysBin" -ForegroundColor Red
    Write-Host "Install from https://www.msys2.org/ then run the prereq pacman commands." -ForegroundColor Yellow
    exit 1
}

# Verify required tools
foreach ($tool in @("gcc", "cargo", "cmake", "nasm")) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        Write-Host "ERROR: '$tool' not found on PATH. Install it via MSYS2 UCRT64." -ForegroundColor Red
        exit 1
    }
}

$LanceRepo = "https://github.com/sysadminmike/lancedb-go.git"
$LanceDir = Join-Path $PSScriptRoot "_lancedb-go"
$RustDir  = Join-Path $LanceDir "rust"
$DestDir  = Join-Path $PSScriptRoot "lib\windows_amd64"

# Use a short CARGO_TARGET_DIR to avoid Windows MAX_PATH issues.
# The aws-lc-sys crate creates deeply nested paths that exceed 260 chars
# when built inside the project tree.
$env:CARGO_TARGET_DIR = "C:\ct"

# Clone or update the lancedb-go repo
if (Test-Path $LanceDir) {
    Write-Host "Updating lancedb-go repo..." -ForegroundColor Yellow
    Push-Location $LanceDir
    git pull --ff-only 2>&1 | Out-Null
    Pop-Location
} else {
    Write-Host "Cloning lancedb-go from $LanceRepo ..." -ForegroundColor Yellow
    git clone $LanceRepo $LanceDir
}

Write-Host "=== Building lancedb-go native library ===" -ForegroundColor Cyan
Write-Host "  Rust source:     $RustDir"
Write-Host "  Target dir:      $env:CARGO_TARGET_DIR  (short path to avoid MAX_PATH)"
Write-Host "  Output:          $DestDir\liblancedb_go.a"
Write-Host "  GCC:             $(gcc --version 2>&1 | Select-Object -First 1)"
Write-Host "  Cargo:           $(cargo --version)"
Write-Host ""

Push-Location $RustDir

if ($Clean) {
    Write-Host "Cleaning previous build..." -ForegroundColor Yellow
    cargo clean 2>&1 | Out-Null
}

Write-Host "Starting cargo build --release (this takes ~20 min on first build)..." -ForegroundColor Yellow
Write-Host ""

cargo build --release
$exitCode = $LASTEXITCODE

Pop-Location

if ($exitCode -ne 0) {
    Write-Host ""
    Write-Host "BUILD FAILED (exit code $exitCode)" -ForegroundColor Red
    Write-Host ""
    Write-Host "Common fixes:" -ForegroundColor Yellow
    Write-Host "  - Missing cmake/nasm/protoc: install via MSYS2 (see .NOTES above)" -ForegroundColor White
    Write-Host "  - Path too long errors: CARGO_TARGET_DIR is already set to C:\ct" -ForegroundColor White
    Write-Host "  - SSL/git errors: add [net] git-fetch-with-cli = true to ~/.cargo/config.toml" -ForegroundColor White
    exit 1
}

# Copy the built library
$builtLib = Join-Path $env:CARGO_TARGET_DIR "release\liblancedb_go.a"
if (Test-Path $builtLib) {
    New-Item -ItemType Directory -Force -Path $DestDir | Out-Null
    Copy-Item $builtLib "$DestDir\liblancedb_go.a" -Force
    $sizeMB = [math]::Round((Get-Item "$DestDir\liblancedb_go.a").Length / 1MB)

    # Sync header from the cloned repo
    $srcHeader = Join-Path $LanceDir "include\lancedb.h"
    $dstHeader = Join-Path $PSScriptRoot "include\lancedb.h"
    if (Test-Path $srcHeader) {
        Copy-Item $srcHeader $dstHeader -Force
        Write-Host "  Header synced: $dstHeader" -ForegroundColor Green
    }

    Write-Host ""
    Write-Host "SUCCESS: Copied to $DestDir\liblancedb_go.a ($sizeMB MB)" -ForegroundColor Green
    Write-Host ""
    Write-Host "You can now build the Go server:" -ForegroundColor Cyan
    Write-Host "  cd $(Split-Path $PSScriptRoot)" -ForegroundColor White
    Write-Host "  .\build.ps1 server" -ForegroundColor White
} else {
    Write-Host "ERROR: Expected $builtLib but file not found after build." -ForegroundColor Red
    exit 1
}
