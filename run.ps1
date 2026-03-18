<#
.SYNOPSIS
    RSS-Lance runtime commands (daily use)

.DESCRIPTION
    Lightweight script for running the fetcher, server, and admin tasks.
    Activates the Python venv automatically. Does NOT handle building
    or first-time setup - use build.ps1 for that.

.EXAMPLE
    .\run.ps1 fetch-once          # Fetch articles once and exit
    .\run.ps1 fetch-daemon        # Run fetcher continuously
    .\run.ps1 server              # Start the HTTP server
    .\run.ps1 demo-data           # Insert demo RSS feeds
    .\run.ps1 add-feed <url>      # Add a single feed URL
    .\run.ps1 export-opml out.opml # Export feeds to OPML
    .\run.ps1 datafix strip-chrome # Fix existing article data
#>

param(
    [Parameter(Position = 0)]
    [ValidateSet("fetch-once", "fetch-daemon", "server", "demo-data", "add-feed", "datafix", "export-opml", "benchmark", "help")]
    [string]$Command = "help",

    [string]$DebugLog = "",

    [int]$Port = 0,

    [Parameter(Position = 1, ValueFromRemainingArguments)]
    [string[]]$ExtraArgs
)

$ErrorActionPreference = "Stop"
$ProjectRoot = $PSScriptRoot

$VenvPath   = Join-Path $ProjectRoot ".venv"
$BuildDir   = Join-Path $ProjectRoot "build"
$DataDir    = Join-Path $ProjectRoot "data"
$FetcherDir = Join-Path $ProjectRoot "fetcher"

function Ensure-Venv {
    $pyExe = Join-Path $VenvPath "Scripts\python.exe"
    if (-not (Test-Path $pyExe)) {
        Write-Host "Python venv not found at: $VenvPath" -ForegroundColor Red
        Write-Host "Run build.ps1 setup first." -ForegroundColor Yellow
        exit 1
    }
    & "$VenvPath\Scripts\Activate.ps1"
}

function Cmd-FetchOnce {
    Ensure-Venv
    Write-Host "Fetching articles (one-shot) ..." -ForegroundColor Cyan
    python "$FetcherDir\main.py" --once
}

function Cmd-FetchDaemon {
    Ensure-Venv
    Write-Host "Starting feed fetcher daemon ..." -ForegroundColor Cyan
    python "$FetcherDir\main.py"
}

function Cmd-Server {
    $exe = Join-Path $BuildDir "rss-lance-server.exe"
    if (-not (Test-Path $exe)) {
        Write-Host "Server not built. Run build.ps1 server first." -ForegroundColor Red
        exit 1
    }

    # Parse host/port from config.toml so we can show the URL up front
    $configFile = Join-Path $ProjectRoot "config.toml"
    $serverHost = "127.0.0.1"
    $serverPort = "8080"
    if (Test-Path $configFile) {
        $toml = Get-Content $configFile -Raw
        if ($toml -match '(?m)^\s*host\s*=\s*"([^"]+)"') { $serverHost = $Matches[1] }
        if ($toml -match '(?m)^\s*port\s*=\s*(\d+)')     { $serverPort = $Matches[1] }
    }

    Write-Host "Loading RSS-Lance server (please wait) ..." -ForegroundColor Cyan
    Write-Host ""

    $serverArgs = @()
    if ($DebugLog) {
        $serverArgs += "--debug", $DebugLog
        Write-Host "Debug: $DebugLog" -ForegroundColor Magenta
    }
    if ($Port -gt 0) {
        $serverArgs += "--port", $Port
        Write-Host "Port override: $Port" -ForegroundColor Magenta
    }
    & $exe @serverArgs
}

function Cmd-DemoData {
    Ensure-Venv
    Write-Host "Inserting demo feeds ..." -ForegroundColor Cyan
    python "$FetcherDir\demo_feeds.py" --data "$DataDir"
}

function Cmd-AddFeed {
    if (-not $ExtraArgs -or $ExtraArgs.Count -eq 0) {
        Write-Host "Usage: .\run.ps1 add-feed <url>" -ForegroundColor Yellow
        exit 1
    }
    Ensure-Venv
    Write-Host "Adding feed: $($ExtraArgs[0])" -ForegroundColor Cyan
    python "$FetcherDir\main.py" --add $ExtraArgs[0]
}

function Cmd-ExportOpml {
    if (-not $ExtraArgs -or $ExtraArgs.Count -eq 0) {
        Write-Host "Usage: .\run.ps1 export-opml <output.opml>" -ForegroundColor Yellow
        Write-Host "  Use '-' to write to stdout" -ForegroundColor Yellow
        exit 1
    }
    Ensure-Venv
    $MigrateDir = Join-Path $ProjectRoot "migrate"
    Write-Host "Exporting OPML ..." -ForegroundColor Cyan
    $passArgs = @($ExtraArgs[0])
    if ($ExtraArgs.Count -gt 1) {
        $passArgs += $ExtraArgs[1..($ExtraArgs.Count - 1)]
    }
    python "$MigrateDir\export_opml.py" @passArgs
}

function Cmd-DataFix {
    if (-not $ExtraArgs -or $ExtraArgs.Count -eq 0) {
        Ensure-Venv
        python "$FetcherDir\datafix.py" list
        return
    }
    Ensure-Venv
    $fixName = $ExtraArgs[0]
    $passArgs = @("--data", $DataDir)
    if ($ExtraArgs.Count -gt 1) {
        $passArgs += $ExtraArgs[1..($ExtraArgs.Count - 1)]
    }
    Write-Host "Running datafix: $fixName" -ForegroundColor Cyan
    python "$FetcherDir\datafix.py" $fixName @passArgs
}

function Cmd-Benchmark {
    Ensure-Venv
    Write-Host "Running benchmark ..." -ForegroundColor Cyan
    python "$ProjectRoot\benchmark.py" @ExtraArgs
}

function Cmd-Help {
    Write-Host @"

RSS-Lance Runtime Commands
==========================
Usage: .\run.ps1 <command> [args]

Commands:
  fetch-once     Fetch all due feeds once and exit
  fetch-daemon   Run the fetcher continuously on a schedule
  server         Start the HTTP server (http://127.0.0.1:8080)
  demo-data      Insert demo RSS feeds for testing
  add-feed <url> Add a single RSS/Atom feed URL
  export-opml <file> Export all feeds to an OPML file (use '-' for stdout)
  datafix <name> Run a data fix on existing articles (or 'datafix' to list)
  benchmark      Run insertion & read benchmarks (isolated temp DB)
  help           Show this help

Options:
  -DebugLog <categories>  Enable debug logging (client,duckdb,batch,lance,all)
  -Port <number>          Override server port from config.toml

Examples:
  .\run.ps1 server -DebugLog all
  .\run.ps1 server -Port 9090
  .\run.ps1 server -DebugLog client,duckdb
  .\run.ps1 datafix strip-chrome
  .\run.ps1 datafix strip-chrome --dry-run
  .\run.ps1 datafix strip-chrome --all

"@ -ForegroundColor Yellow
}

switch ($Command) {
    "fetch-once"   { Cmd-FetchOnce }
    "fetch-daemon" { Cmd-FetchDaemon }
    "server"       { Cmd-Server }
    "demo-data"    { Cmd-DemoData }
    "add-feed"     { Cmd-AddFeed }
    "export-opml"  { Cmd-ExportOpml }
    "datafix"      { Cmd-DataFix }
    "benchmark"    { Cmd-Benchmark }
    "help"         { Cmd-Help }
}
