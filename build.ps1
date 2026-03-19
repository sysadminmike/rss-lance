<#
.SYNOPSIS
    RSS-Lance build & setup script for Windows (PowerShell)

.DESCRIPTION
    Handles Python venv setup, Go builds, and cross-compilation.
    Accepts an optional -Dir parameter to specify the project directory
    (defaults to the directory where this script lives).

.EXAMPLE
    .\build.ps1 setup                        # Setup using script directory
    .\build.ps1 -Dir C:\src\rss-lance setup   # Setup in a specific directory
    .\build.ps1 server                        # Build Go server for current platform
    .\build.ps1 server-all                    # Cross-compile server for all platforms
    .\build.ps1 fetcher                       # Install fetcher Python deps
    .\build.ps1 run-fetcher                   # Run the feed fetcher
    .\build.ps1 run-server                    # Run the HTTP server
    .\build.ps1 demo-data                     # Insert demo RSS feeds for testing
    .\build.ps1 duckdb                        # Download DuckDB CLI into tools/
    .\build.ps1 migrate                       # Install migration deps
    .\build.ps1 clean                         # Clean build artifacts
    .\build.ps1 release                       # Package release zip for distribution
    .\build.ps1 all                           # Full build (setup + duckdb + server + demo-data)
#>

param(
    [Parameter(Position = 0)]
    [ValidateSet("setup", "server", "server-all", "fetcher", "run-fetcher", "fetch-once", "run-server", "demo-data", "duckdb", "migrate", "migrate-cleanup", "test", "clean", "release", "all", "minimum", "help")]
    [string]$Command = "help",

    [Parameter()]
    [Alias("Directory")]
    [string]$Dir,

    [Parameter()]
    [switch]$LanceEmbedded,

    [Parameter()]
    [switch]$LanceExternal,

    [Parameter()]
    [switch]$NoTests
)

$ErrorActionPreference = "Stop"

# Source root is always where this script lives
$SourceRoot = $PSScriptRoot

# Output root: use -Dir if provided, otherwise same as source root
if ($Dir) {
    # Create the directory if it doesn't exist, then resolve
    if (-not (Test-Path $Dir)) {
        New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    }
    $ProjectRoot = (Resolve-Path $Dir).Path
} else {
    $ProjectRoot = $SourceRoot
}

Write-Host "Source from: $SourceRoot" -ForegroundColor DarkGray
Write-Host "Working in:  $ProjectRoot" -ForegroundColor DarkGray

$VenvPath   = Join-Path $ProjectRoot ".venv"
$BuildDir   = Join-Path $ProjectRoot "build"
$DataDir    = Join-Path $ProjectRoot "data"
$ToolsDir   = Join-Path $ProjectRoot "tools"

# Source directory for Go compilation (only needed at build time)
$ServerDir  = Join-Path $SourceRoot "server"

# Runtime directories -- always relative to $ProjectRoot so the app
# works after the source repo is deleted.
$FetcherDir = Join-Path $ProjectRoot "fetcher"
$FrontendDir = Join-Path $ProjectRoot "frontend"
$MigrateDir = Join-Path $ProjectRoot "migrate"
$ConfigFile = Join-Path $ProjectRoot "config.toml"

function Write-Step($msg) { Write-Host "`n== $msg ==" -ForegroundColor Cyan }

function Copy-RuntimeFiles {
    # Copy the Python scripts, frontend, and config into $ProjectRoot
    # so the app is self-contained and works after deleting the source repo.
    if ($SourceRoot -eq $ProjectRoot) { return }

    Write-Step "Copying runtime files to project directory"

    # fetcher/*.py + requirements.txt (no __pycache__)
    $destFetcher = Join-Path $ProjectRoot "fetcher"
    New-Item -ItemType Directory -Force -Path $destFetcher | Out-Null
    Get-ChildItem "$SourceRoot\fetcher" -File | Where-Object { $_.Name -ne '__pycache__' } |
        ForEach-Object { Copy-Item $_.FullName "$destFetcher\$($_.Name)" -Force }
    Write-Host "  Copied fetcher/ ($(( Get-ChildItem $destFetcher -File ).Count) files)"

    # frontend/ (recursive)
    $srcFrontend = Join-Path $SourceRoot "frontend"
    if (Test-Path $srcFrontend) {
        Copy-Item $srcFrontend $ProjectRoot -Recurse -Force
        Write-Host "  Copied frontend/"
    }

    # config.toml (only if not already present in target)
    $srcConfig = Join-Path $SourceRoot "config.toml"
    if ((Test-Path $srcConfig) -and -not (Test-Path $ConfigFile)) {
        Copy-Item $srcConfig $ConfigFile -Force
        Write-Host "  Copied config.toml"
    }

    # run.ps1 / run.sh (runtime scripts)
    foreach ($runScript in @("run.ps1", "run.sh")) {
        $srcRun = Join-Path $SourceRoot $runScript
        if (Test-Path $srcRun) {
            Copy-Item $srcRun (Join-Path $ProjectRoot $runScript) -Force
        }
    }
    Write-Host "  Copied run.ps1 / run.sh"
}

function Ensure-Venv {
    if (-not (Test-Path "$VenvPath\Scripts\python.exe")) {
        Write-Step "Creating Python virtual environment"
        python -m venv $VenvPath
    }
    # Activate for this session
    & "$VenvPath\Scripts\Activate.ps1"
}

function Install-Requirements($ReqFile) {
    $prevPref = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    # Fast path: try installing everything at once
    $output = pip install -r $ReqFile 2>&1
    if ($LASTEXITCODE -eq 0) { $ErrorActionPreference = $prevPref; return }

    Write-Host "Bulk install failed, retrying per-package with binary fallback..." -ForegroundColor Yellow
    foreach ($line in Get-Content $ReqFile) {
        $line = $line.Trim()
        if (-not $line -or $line.StartsWith('#')) { continue }
        $output = pip install $line 2>&1
        if ($LASTEXITCODE -ne 0 -and $line -match '-binary') {
            $fallback = $line -replace '-binary', ''
            Write-Host "  Binary failed for $line, trying $fallback" -ForegroundColor Yellow
            pip install $fallback
        } elseif ($LASTEXITCODE -ne 0) {
            $ErrorActionPreference = $prevPref
            Write-Host "Failed to install: $line" -ForegroundColor Red
            exit 1
        }
    }
    $ErrorActionPreference = $prevPref
}

function Setup {
    # Copy runtime files first so pip install reads from $ProjectRoot
    Copy-RuntimeFiles

    Write-Step "Setting up Python virtual environment"
    Ensure-Venv

    Write-Step "Installing fetcher dependencies"
    Install-Requirements "$FetcherDir\requirements.txt"

    Write-Step "Verifying Go installation"
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    if ($goCmd) {
        go version
    } else {
        Write-Host "Go is not installed. Server builds will fail." -ForegroundColor Yellow
        Write-Host "Install with: winget install GoLang.Go" -ForegroundColor Yellow
    }

    Write-Step "Initializing Go module (if needed)"
    if (-not (Test-Path "$ServerDir\go.mod")) {
        Push-Location $ServerDir
        go mod init rss-lance/server
        Pop-Location
    }

    Write-Step "Creating data directory"
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

    Write-Host "`nSetup complete!" -ForegroundColor Green
}

function Ensure-Gcc {
    # CGo (required by lancedb-go) needs a C compiler on Windows.
    $gcc = Get-Command gcc -ErrorAction SilentlyContinue
    if ($gcc) { return }

    # Check common MSYS2 / MinGW paths not yet on PATH
    $candidates = @(
        "C:\msys64\ucrt64\bin",
        "C:\msys64\mingw64\bin",
        "C:\mingw64\bin"
    )
    foreach ($dir in $candidates) {
        if (Test-Path "$dir\gcc.exe") {
            Write-Host "Found GCC at $dir -- adding to PATH for this session" -ForegroundColor Yellow
            $env:PATH = "$dir;" + $env:PATH
            return
        }
    }

    Write-Host ""
    Write-Host "ERROR: GCC (C compiler) is required to build the server but was not found." -ForegroundColor Red
    Write-Host ""
    Write-Host "The Go server uses CGo (lancedb-go native bindings) which needs a C compiler." -ForegroundColor Yellow
    Write-Host "End users do NOT need this -- only developers compiling from source." -ForegroundColor Yellow
    Write-Host ""
    Write-Host "To install GCC on Windows:" -ForegroundColor Cyan
    Write-Host "  1. Download & install MSYS2 from https://www.msys2.org/" -ForegroundColor White
    Write-Host "  2. Open the 'MSYS2 UCRT64' terminal and run:" -ForegroundColor White
    Write-Host "       pacman -S mingw-w64-ucrt-x86_64-gcc" -ForegroundColor White
    Write-Host "  3. Add C:\msys64\ucrt64\bin to your system PATH" -ForegroundColor White
    Write-Host "  4. Re-open PowerShell and run this build script again" -ForegroundColor White
    Write-Host ""
    exit 1
}

function Build-Server {
    Write-Step "Building Go server (Windows)"
    Ensure-Gcc
    New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
    Push-Location $ServerDir

    # Enable CGo and point at lancedb-go native library
    # -static ensures MinGW runtime (libwinpthread, libgcc) is linked into the
    # binary so it runs standalone without needing MSYS2 DLLs on PATH.
    $env:CGO_ENABLED = "1"
    $env:CGO_CFLAGS  = "-I$ServerDir\include"
    $env:CGO_LDFLAGS = "-static $ServerDir\lib\windows_amd64\liblancedb_go.a -lws2_32 -luserenv -lntdll -lpthread"

    $buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    $ldflags = "-X main.BuildTime=$buildTime"
    if ($env:BUILD_VERSION) { $ldflags += " -X main.BuildVersion=$env:BUILD_VERSION" }

    # Capture DuckDB CLI + Lance extension versions at build time
    $duckBin = Join-Path $ToolsDir "duckdb.exe"
    if (Test-Path $duckBin) {
        try {
            $verJson = & $duckBin -json -c "SELECT version() AS v" 2>$null | Out-String
            $verObj = $verJson | ConvertFrom-Json
            if ($verObj -and $verObj[0].v) {
                $duckVer = $verObj[0].v
                $ldflags += " -X main.BuildDuckDBVersion=$duckVer"
                Write-Host "  DuckDB CLI version: $duckVer"
            }
        } catch {
            Write-Host "  WARNING: Could not detect DuckDB version" -ForegroundColor Yellow
        }
        try {
            $extJson = & $duckBin -json -c "INSTALL lance FROM community; LOAD lance; SELECT extension_version FROM duckdb_extensions() WHERE extension_name='lance' AND loaded=true" 2>$null | Out-String
            $extObj = $extJson | ConvertFrom-Json
            if ($extObj -and $extObj[0].extension_version) {
                $lanceVer = $extObj[0].extension_version
                $ldflags += " -X main.BuildLanceExtVersion=$lanceVer"
                Write-Host "  Lance extension version: $lanceVer"
            }
        } catch {
            Write-Host "  WARNING: Could not detect Lance extension version" -ForegroundColor Yellow
        }
    } else {
        Write-Host "  NOTE: tools\duckdb.exe not found, skipping build-time version capture" -ForegroundColor Yellow
    }

    go build -ldflags "$ldflags" -o "$BuildDir\rss-lance-server.exe" .

    # Reset CGo env
    Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_CFLAGS  -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_LDFLAGS -ErrorAction SilentlyContinue

    Pop-Location
    Write-Host "Built: build\rss-lance-server.exe" -ForegroundColor Green
}

function Build-ServerAll {
    Write-Step "Cross-compiling Go server for all platforms"
    Ensure-Gcc
    New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
    Push-Location $ServerDir

    # Native Windows build (CGo enabled for lancedb-go)
    $outName = "rss-lance-server-windows-amd64.exe"
    Write-Host "  Building $outName (CGo) ..."
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "1"
    $env:CGO_CFLAGS  = "-I$ServerDir\include"
    $env:CGO_LDFLAGS = "-static $ServerDir\lib\windows_amd64\liblancedb_go.a -lws2_32 -luserenv -lntdll -lpthread"
    $buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    $ldflags = "-X main.BuildTime=$buildTime"
    if ($env:BUILD_VERSION) { $ldflags += " -X main.BuildVersion=$env:BUILD_VERSION" }

    # Capture DuckDB CLI + Lance extension versions at build time
    $duckBin = Join-Path $ToolsDir "duckdb.exe"
    if (Test-Path $duckBin) {
        try {
            $verJson = & $duckBin -json -c "SELECT version() AS v" 2>$null | Out-String
            $verObj = $verJson | ConvertFrom-Json
            if ($verObj -and $verObj[0].v) {
                $duckVer = $verObj[0].v
                $ldflags += " -X main.BuildDuckDBVersion=$duckVer"
                Write-Host "  DuckDB CLI version: $duckVer"
            }
        } catch {
            Write-Host "  WARNING: Could not detect DuckDB version" -ForegroundColor Yellow
        }
        try {
            $extJson = & $duckBin -json -c "INSTALL lance FROM community; LOAD lance; SELECT extension_version FROM duckdb_extensions() WHERE extension_name='lance' AND loaded=true" 2>$null | Out-String
            $extObj = $extJson | ConvertFrom-Json
            if ($extObj -and $extObj[0].extension_version) {
                $lanceVer = $extObj[0].extension_version
                $ldflags += " -X main.BuildLanceExtVersion=$lanceVer"
                Write-Host "  Lance extension version: $lanceVer"
            }
        } catch {
            Write-Host "  WARNING: Could not detect Lance extension version" -ForegroundColor Yellow
        }
    } else {
        Write-Host "  NOTE: tools\duckdb.exe not found, skipping build-time version capture" -ForegroundColor Yellow
    }

    go build -ldflags "$ldflags" -o "$BuildDir\$outName" .

    # Cross-compiled targets -- CGo disabled (need native libs per-platform in CI)
    $crossTargets = @(
        @{ GOOS = "linux";   GOARCH = "amd64"; Ext = "" },
        @{ GOOS = "linux";   GOARCH = "arm64"; Ext = "" },
        @{ GOOS = "darwin";  GOARCH = "amd64"; Ext = "" },
        @{ GOOS = "darwin";  GOARCH = "arm64"; Ext = "" },
        @{ GOOS = "freebsd"; GOARCH = "amd64"; Ext = "" }
    )

    Write-Host "  Cross-compiled targets require platform-specific native libs." -ForegroundColor Yellow
    Write-Host "  Build these on their native platform or in CI instead." -ForegroundColor Yellow

    # Reset env
    Remove-Item Env:\GOOS       -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH     -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_CFLAGS  -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_LDFLAGS -ErrorAction SilentlyContinue

    Pop-Location
    Write-Host "All builds in: build\" -ForegroundColor Green
}

function Install-Fetcher {
    Ensure-Venv
    Write-Step "Installing fetcher dependencies"
    Install-Requirements "$FetcherDir\requirements.txt"
}

function Run-Fetcher {
    Ensure-Venv
    Write-Step "Running feed fetcher"
    python "$FetcherDir\main.py"
}

function Fetch-Once {
    Ensure-Venv
    Write-Step "Fetching articles once"
    python "$FetcherDir\main.py" --once
}

function Run-Server {
    $exe = "$BuildDir\rss-lance-server.exe"
    if (-not (Test-Path $exe)) {
        Write-Host "Server not built yet. Run: .\build.ps1 server" -ForegroundColor Red
        exit 1
    }
    Write-Step "Starting HTTP server"
    & $exe
}

function Run-Migrate {
    # Copy migrate scripts from source if needed (one-off operation)
    if (-not (Test-Path "$MigrateDir\common.py")) {
        $srcMigrate = Join-Path $SourceRoot "migrate"
        if (Test-Path $srcMigrate) {
            New-Item -ItemType Directory -Force -Path $MigrateDir | Out-Null
            Get-ChildItem $srcMigrate -File |
                ForEach-Object { Copy-Item $_.FullName "$MigrateDir\$($_.Name)" -Force }
            Write-Host "  Copied migrate/ scripts" -ForegroundColor DarkGray
        } else {
            Write-Host "migrate/ directory not found in source or project." -ForegroundColor Red
            exit 1
        }
    }

    Ensure-Venv
    Write-Step "Installing migration dependencies"
    Install-Requirements "$MigrateDir\requirements.txt"

    Write-Host ""
    Write-Host "Migration deps installed. Run an importer directly:" -ForegroundColor Green
    Write-Host "  python migrate\import_ttrss.py          # TT-RSS (Postgres)" -ForegroundColor Cyan
    Write-Host "  python migrate\import_freshrss.py       # FreshRSS (API)" -ForegroundColor Cyan
    Write-Host "  python migrate\import_miniflux.py       # Miniflux (API)" -ForegroundColor Cyan
    Write-Host "  python migrate\import_opml.py <file>    # OPML (any reader)" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "See docs/importing.md for configuration details." -ForegroundColor Yellow
}

function Migrate-Cleanup {
    Write-Step "Cleaning up migration files"

    # Remove migrate/ directory
    if (Test-Path $MigrateDir) {
        Remove-Item -Recurse -Force $MigrateDir
        Write-Host "  Removed migrate/" -ForegroundColor Green
    } else {
        Write-Host "  migrate/ not found (already clean)" -ForegroundColor Yellow
    }

    # Uninstall migration-only deps
    Ensure-Venv
    Write-Host "  Uninstalling migration dependencies ..."
    pip uninstall -y psycopg2-binary tqdm requests 2>$null
    Write-Host "  Done." -ForegroundColor Green
}

function Install-DuckDB {
    Write-Step "Downloading DuckDB CLI"
    New-Item -ItemType Directory -Force -Path $ToolsDir | Out-Null
    $duckExe = Join-Path $ToolsDir "duckdb.exe"
    if (Test-Path $duckExe) {
        Write-Host "duckdb.exe already exists in tools/" -ForegroundColor Yellow
        return
    }
    $ver = "v1.5.0"
    $url = "https://github.com/duckdb/duckdb/releases/download/$ver/duckdb_cli-windows-amd64.zip"
    $zip = Join-Path $ToolsDir "duckdb.zip"
    Write-Host "Downloading DuckDB $ver ..."
    Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
    Expand-Archive -Path $zip -DestinationPath $ToolsDir -Force
    Remove-Item $zip
    if (Test-Path $duckExe) {
        Write-Host "Installed: tools\duckdb.exe" -ForegroundColor Green
    } else {
        Write-Host "Warning: duckdb.exe not found after extraction" -ForegroundColor Red
    }
}

function Insert-DemoData {
    Ensure-Venv
    Write-Step "Inserting demo RSS feeds"
    python "$FetcherDir\demo_feeds.py" --data "$DataDir"
}

function Build-Release {
    Write-Step "Building release package"

    # Build the server first
    Build-Server

    # Ensure DuckDB CLI is available
    Install-DuckDB

    $ReleaseDir = Join-Path $ProjectRoot "release"
    $StagingDir = Join-Path $ReleaseDir "rss-lance"

    # Clean previous staging
    if (Test-Path $StagingDir) { Remove-Item -Recurse -Force $StagingDir }
    New-Item -ItemType Directory -Force -Path $StagingDir | Out-Null

    # Copy server exe
    Copy-Item "$BuildDir\rss-lance-server.exe" $StagingDir
    Write-Host "  Copied rss-lance-server.exe" -ForegroundColor DarkGray

    # Copy DuckDB CLI
    $duckSrc = Join-Path $ToolsDir "duckdb.exe"
    if (Test-Path $duckSrc) {
        $toolsDest = Join-Path $StagingDir "tools"
        New-Item -ItemType Directory -Force -Path $toolsDest | Out-Null
        Copy-Item $duckSrc $toolsDest
        Write-Host "  Copied tools/duckdb.exe" -ForegroundColor DarkGray
    }

    # Copy frontend
    $srcFrontend = Join-Path $SourceRoot "frontend"
    if (Test-Path $srcFrontend) {
        $destFrontend = Join-Path $StagingDir "frontend"
        Copy-Item $srcFrontend $destFrontend -Recurse -Exclude "node_modules","package.json","jest.config.js","tests"
        Write-Host "  Copied frontend/" -ForegroundColor DarkGray
    }

    # Copy fetcher (Python scripts only, no __pycache__)
    $destFetcher = Join-Path $StagingDir "fetcher"
    New-Item -ItemType Directory -Force -Path $destFetcher | Out-Null
    Get-ChildItem "$SourceRoot\fetcher" -File |
        ForEach-Object { Copy-Item $_.FullName "$destFetcher\$($_.Name)" -Force }
    Write-Host "  Copied fetcher/" -ForegroundColor DarkGray

    # Copy config and run scripts
    foreach ($f in @("config.toml", "run.ps1", "run.sh", "README.md", "LICENSE")) {
        $src = Join-Path $SourceRoot $f
        if (Test-Path $src) {
            Copy-Item $src $StagingDir
        }
    }
    Write-Host "  Copied config.toml, run scripts, README, LICENSE" -ForegroundColor DarkGray

    # Create the zip
    $zipName = "rss-lance-windows-amd64.zip"
    $zipPath = Join-Path $ReleaseDir $zipName
    if (Test-Path $zipPath) { Remove-Item $zipPath }
    Compress-Archive -Path $StagingDir -DestinationPath $zipPath

    # Clean up staging directory
    Remove-Item -Recurse -Force $StagingDir

    $sizeMB = [math]::Round((Get-Item $zipPath).Length / 1MB, 1)
    Write-Host ""
    Write-Host "Release package created: release\$zipName ($sizeMB MB)" -ForegroundColor Green
    Write-Host "  Upload this zip to GitHub Releases." -ForegroundColor Yellow
    Write-Host "  Users unzip and run: .\run.ps1 server" -ForegroundColor Yellow
}

function Clean {
    Write-Step "Cleaning build artifacts"
    if (Test-Path $BuildDir) { Remove-Item -Recurse -Force $BuildDir }
    if (Test-Path (Join-Path $ProjectRoot "release")) { Remove-Item -Recurse -Force (Join-Path $ProjectRoot "release") }
    Write-Host "Cleaned." -ForegroundColor Green
}

function Run-Tests {
    Write-Step "Running test suite"
    $testScript = Join-Path $SourceRoot "test.ps1"
    if (-not (Test-Path $testScript)) {
        Write-Host "  test.ps1 not found at $testScript" -ForegroundColor Red
        return
    }
    & $testScript all
    if ($LASTEXITCODE -ne 0) {
        Write-Host "  Some tests failed (see output above)" -ForegroundColor Yellow
    }
}

function Show-Help {
    Write-Host @"

RSS-Lance Build Script
======================
Usage: .\build.ps1 [-Dir <path>] [-NoTests] <command>

Options:
  -Dir <path>  Project directory to work in (default: script location)
  -NoTests     Skip running tests after build (tests run by default with 'all')

Commands:
  setup        First-time setup (venv + deps + Go check)
  server       Build Go server for Windows
  server-all   Cross-compile server for Windows/Linux/macOS/FreeBSD
  fetcher      Install Python fetcher dependencies
  run-fetcher  Run the feed fetcher daemon
  fetch-once   Fetch articles once and exit
  run-server   Run the HTTP server
  demo-data    Insert demo RSS feeds into LanceDB for testing
  duckdb       Download DuckDB CLI into tools/
  migrate      Install migration deps (then run an importer directly)
  migrate-cleanup  Remove migrate scripts and their deps
  test         Run all test suites (or use test.ps1 directly)
  clean        Remove build artifacts
  release      Build + package a release zip (exe + frontend + fetcher + config)
  minimum      Bare minimum to run the app (setup + duckdb + server)
               No tests, no demo data, no Node.js needed
  all          Full build (setup + duckdb + server + demo-data + tests)
               Use -NoTests to skip tests: .\build.ps1 -NoTests all
  help         Show this help

Examples:
  .\build.ps1 setup                        # Use script directory
  .\build.ps1 -Dir C:\src\rss-lance all    # Build in a specific dir
  .\build.ps1 -NoTests all                 # Build without running tests
  .\build.ps1 test                         # Run tests only

"@ -ForegroundColor Yellow
}

# Ensure data directory exists for every command (tests, server, etc. may need it)
New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

switch ($Command) {
    "setup"      { Setup }
    "server"     { Build-Server }
    "server-all" { Build-ServerAll }
    "fetcher"    { Install-Fetcher }
    "run-fetcher" { Run-Fetcher }
    "fetch-once" { Fetch-Once }
    "run-server" { Run-Server }
    "demo-data"  { Insert-DemoData }
    "duckdb"     { Install-DuckDB }
    "migrate"    { Run-Migrate }
    "migrate-cleanup" { Migrate-Cleanup }
    "test"       { Run-Tests }
    "clean"      { Clean }
    "release"    { Build-Release }
    "minimum" {
        Setup; Install-DuckDB; Build-Server
        Write-Host ""
        Write-Host "Minimum build complete. Your app is ready to run:" -ForegroundColor Green
        Write-Host "  1. Fetch articles:  .\run.ps1 fetch-once" -ForegroundColor Cyan
        Write-Host "  2. Start server:    .\run.ps1 server" -ForegroundColor Cyan
        Write-Host "  3. Open browser:    http://127.0.0.1:8080" -ForegroundColor Cyan
        Write-Host ""
        Write-Host "Optional: insert demo feeds with  .\build.ps1 demo-data" -ForegroundColor Yellow
    }
    "all"        {
        Setup; Install-DuckDB; Build-Server; Insert-DemoData
        if (-not $NoTests) { Run-Tests }
    }
    "help"       { Show-Help }
}

# Remind user to cd into the project directory when -Dir was used
if ($Dir -and $Command -ne "help") {
    Write-Host ""
    Write-Host "NOTE: Your project directory is self-contained at:" -ForegroundColor Yellow
    Write-Host "    cd $ProjectRoot" -ForegroundColor Cyan
    Write-Host "  Use run.ps1 for daily commands (fetch-once, server, etc.)" -ForegroundColor Yellow
    Write-Host "  You can delete the source repo - the app will keep working." -ForegroundColor Yellow
}
