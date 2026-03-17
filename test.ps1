<#
.SYNOPSIS
    Run RSS-Lance test suites with clear per-test PASS/FAIL output.

.DESCRIPTION
    Runs Python fetcher tests (pytest), Go API + DB tests, and
    frontend tests (Jest, requires Node.js).
    Output is designed to be easy to scan for humans and AI agents.

.EXAMPLE
    .\test.ps1            # Run all tests
    .\test.ps1 python     # Python fetcher tests only
    .\test.ps1 go         # Go API + DB tests only
    .\test.ps1 frontend   # Frontend tests only (requires Node.js)
    .\test.ps1 backend    # Python + Go (no frontend)
    .\test.ps1 database   # Python DB integration tests only
    .\test.ps1 api        # Go API tests only
#>

param(
    [Parameter(Position = 0)]
    [ValidateSet("all", "python", "go", "frontend", "backend", "database", "api", "help")]
    [string]$Suite = "all"
)

$ErrorActionPreference = "Stop"
$ProjectRoot = $PSScriptRoot
$ServerDir   = Join-Path $ProjectRoot "server"

# ── Counters ───────────────────────────────────────────────────────────────────

$script:totalPassed  = 0
$script:totalFailed  = 0
$script:totalSkipped = 0
$script:failedTests  = @()
$script:suitesRun    = @()

# ── Helpers ────────────────────────────────────────────────────────────────────

function Write-Section($title) {
    Write-Host ""
    Write-Host ("=" * 70) -ForegroundColor Cyan
    Write-Host "  $title" -ForegroundColor Cyan
    Write-Host ("=" * 70) -ForegroundColor Cyan
    Write-Host ""
}

function Write-SubSection($title) {
    Write-Host ""
    Write-Host ("  --- $title ---") -ForegroundColor DarkCyan
    Write-Host ""
}

function Write-TestResult($name, $status, $detail) {
    switch ($status) {
        "PASS" {
            Write-Host "  [PASS] $name" -ForegroundColor Green
            $script:totalPassed++
        }
        "FAIL" {
            Write-Host "  [FAIL] $name" -ForegroundColor Red
            if ($detail) {
                $detail -split "`n" | ForEach-Object {
                    $l = $_.Trim()
                    if ($l) { Write-Host "         $l" -ForegroundColor DarkRed }
                }
            }
            $script:totalFailed++
            $script:failedTests += $name
        }
        "SKIP" {
            Write-Host "  [SKIP] $name" -ForegroundColor Yellow
            $script:totalSkipped++
        }
    }
}

# ── Python Tests ───────────────────────────────────────────────────────────────

function Run-PythonTests {
    param([string[]]$TestPaths = @("fetcher/tests/"))

    $label = if ($TestPaths.Count -eq 1 -and $TestPaths[0] -ne "fetcher/tests/") {
        "Python Tests ($($TestPaths[0]))"
    } else { "Python Fetcher Tests" }

    Write-Section $label
    $script:suitesRun += $label

    $python = Join-Path $ProjectRoot ".venv\Scripts\python.exe"
    if (-not (Test-Path $python)) {
        Write-Host "  [SKIP] Python venv not found at .venv\" -ForegroundColor Yellow
        Write-Host "         Run: .\build.ps1 setup" -ForegroundColor Yellow
        return
    }

    # Ensure pytest is available
    & $python -m pytest --version 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "  Installing pytest..." -ForegroundColor Yellow
        & (Join-Path $ProjectRoot ".venv\Scripts\pip.exe") install pytest -q 2>$null
    }

    Push-Location $ProjectRoot

    # Run pytest with verbose one-line-per-test output
    $rawOutput = & $python -m pytest @TestPaths -v --tb=short --no-header 2>&1 | Out-String

    # Parse pytest verbose lines: "path::Class::method PASSED [ N%]"
    $inFailureBlock = $false
    $failLines = ""
    $failTestName = ""

    foreach ($line in ($rawOutput -split "`n")) {
        $trimmed = $line.Trim()

        if ($trimmed -match '^(.+?)\s+(PASSED|FAILED|SKIPPED|ERROR)\s*(\[.*\])?\s*$') {
            # Flush any pending failure detail
            if ($inFailureBlock -and $failTestName) {
                Write-TestResult $failTestName "FAIL" $failLines
                $inFailureBlock = $false
                $failLines = ""
            }

            $testName = $Matches[1]
            $result   = $Matches[2]

            # Clean up long path for display
            $testName = $testName -replace '^fetcher/tests/', '' -replace '\.py::', ' > '

            switch ($result) {
                "PASSED"  { Write-TestResult $testName "PASS" }
                "SKIPPED" { Write-TestResult $testName "SKIP" }
                "FAILED"  {
                    $failTestName = $testName
                    $inFailureBlock = $true
                    $failLines = ""
                }
                "ERROR" {
                    Write-TestResult $testName "FAIL" "Collection/import error"
                }
            }
        }
        # Capture short traceback lines for failures
        elseif ($inFailureBlock) {
            if ($trimmed -match '^(FAILURES|={5,}|-{5,}|\d+ (passed|failed))') {
                Write-TestResult $failTestName "FAIL" $failLines
                $inFailureBlock = $false
                $failLines = ""
                $failTestName = ""
            } elseif ($trimmed) {
                $failLines += "$trimmed`n"
            }
        }
    }

    # Flush trailing
    if ($inFailureBlock -and $failTestName) {
        Write-TestResult $failTestName "FAIL" $failLines
    }

    Pop-Location
}

# ── Go Tests ───────────────────────────────────────────────────────────────────

function Run-GoTestPackage {
    param([string]$Package, [string]$Label)

    Write-SubSection $Label

    Push-Location $ServerDir

    $rawOutput = go test "./$Package/" -v -count=1 -timeout 300s 2>&1 | Out-String

    # Parse go test -v output: "--- PASS: TestName (0.00s)" / "--- FAIL: TestName (0.00s)"
    $failBlock = ""
    $currentTest = ""
    $inFail = $false

    foreach ($line in ($rawOutput -split "`n")) {
        $trimmed = $line.TrimEnd()

        if ($trimmed -match '^\s*--- PASS:\s+(\S+)\s+\(') {
            if ($inFail -and $currentTest) {
                Write-TestResult "$Package/$currentTest" "FAIL" $failBlock
                $inFail = $false; $failBlock = ""
            }
            Write-TestResult "$Package/$($Matches[1])" "PASS"
        }
        elseif ($trimmed -match '^\s*--- FAIL:\s+(\S+)\s+\(') {
            if ($inFail -and $currentTest) {
                Write-TestResult "$Package/$currentTest" "FAIL" $failBlock
            }
            $currentTest = $Matches[1]
            $inFail = $true
            $failBlock = ""
        }
        elseif ($trimmed -match '^\s*--- SKIP:\s+(\S+)\s+\(') {
            if ($inFail -and $currentTest) {
                Write-TestResult "$Package/$currentTest" "FAIL" $failBlock
                $inFail = $false; $failBlock = ""
            }
            Write-TestResult "$Package/$($Matches[1])" "SKIP"
        }
        elseif ($inFail) {
            if ($trimmed -match '^(FAIL|ok)\s' -or $trimmed -match '^---\s+(PASS|FAIL|SKIP):') {
                Write-TestResult "$Package/$currentTest" "FAIL" $failBlock
                $inFail = $false; $failBlock = ""
            } else {
                $failBlock += "$trimmed`n"
            }
        }
    }

    if ($inFail -and $currentTest) {
        Write-TestResult "$Package/$currentTest" "FAIL" $failBlock
    }

    # Detect build failures
    if ($rawOutput -match 'FAIL\s+.*\[build failed\]') {
        Write-TestResult "$Package (build)" "FAIL" "Compilation failed"
    }

    Pop-Location
}

function Ensure-GoEnv {
    $goExe = Get-Command go -ErrorAction SilentlyContinue
    if (-not $goExe) {
        if (Test-Path "C:\Program Files\Go\bin\go.exe") {
            $env:PATH = "C:\Program Files\Go\bin;$env:PATH"
        } else {
            return $false
        }
    }

    $gccExe = Get-Command gcc -ErrorAction SilentlyContinue
    if (-not $gccExe) {
        foreach ($dir in @("C:\msys64\ucrt64\bin", "C:\msys64\mingw64\bin", "C:\mingw64\bin")) {
            if (Test-Path "$dir\gcc.exe") {
                $env:PATH = "$dir;$env:PATH"
                $gccExe = $true
                break
            }
        }
        if (-not $gccExe) { return $false }
    }

    $env:CGO_ENABLED = "1"
    $env:CGO_CFLAGS  = "-I$ServerDir\include"
    $env:CGO_LDFLAGS = "-static $ServerDir\lib\windows_amd64\liblancedb_go.a -lws2_32 -luserenv -lntdll -lpthread"
    return $true
}

function Cleanup-GoEnv {
    Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_CFLAGS  -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_LDFLAGS -ErrorAction SilentlyContinue
}

function Run-GoTests {
    param([string[]]$Packages = @("api", "db"))

    Write-Section "Go Server Tests"
    $script:suitesRun += "Go Server Tests"

    if (-not (Ensure-GoEnv)) {
        Write-Host "  [SKIP] Go or GCC/MinGW not found (required for CGo)" -ForegroundColor Yellow
        Write-Host "         Install Go: https://go.dev/dl/" -ForegroundColor Yellow
        Write-Host "         Install GCC: pacman -S mingw-w64-ucrt-x86_64-gcc (MSYS2)" -ForegroundColor Yellow
        return
    }

    foreach ($pkg in $Packages) {
        Run-GoTestPackage $pkg "Go $($pkg)/ package"
    }

    Cleanup-GoEnv
}

# ── Frontend Tests ─────────────────────────────────────────────────────────────

function Run-FrontendTests {
    Write-Section "Frontend Tests (Jest)"
    $script:suitesRun += "Frontend Tests"

    $npmExe = Get-Command npm -ErrorAction SilentlyContinue
    if (-not $npmExe) {
        Write-Host "  [SKIP] Node.js/npm not found" -ForegroundColor Yellow
        Write-Host "         Install Node.js, then: cd frontend && npm install && npm test" -ForegroundColor Yellow
        return
    }

    $frontendDir = Join-Path $ProjectRoot "frontend"
    Push-Location $frontendDir

    if (-not (Test-Path "node_modules")) {
        Write-Host "  Installing dependencies..." -ForegroundColor DarkGray
        npm install 2>$null
    }

    $rawOutput = npx jest --verbose --no-color 2>&1 | Out-String

    foreach ($line in ($rawOutput -split "`n")) {
        $trimmed = $line.TrimEnd()

        # Jest suite-level: "PASS tests/file.test.js" or "FAIL tests/file.test.js"
        if ($trimmed -match '^\s*(PASS|FAIL)\s+tests/(.+)$') {
            # Suite header - don't count, individual tests below
        }
        # Jest individual test (verbose): lines with checkmark or cross
        elseif ($trimmed -match '^\s+[√✓]\s+(.+?)(\s+\(\d+\s*m?s\))?\s*$') {
            Write-TestResult $Matches[1] "PASS"
        }
        elseif ($trimmed -match '^\s+[×✕]\s+(.+?)(\s+\(\d+\s*m?s\))?\s*$') {
            Write-TestResult $Matches[1] "FAIL"
        }
        # Fallback: "pass" / "fail" text markers
        elseif ($trimmed -match '^\s+pass\s+(.+)$') {
            Write-TestResult $Matches[1] "PASS"
        }
        elseif ($trimmed -match '^\s+fail\s+(.+)$') {
            Write-TestResult $Matches[1] "FAIL"
        }
    }

    Pop-Location
}

# ── Main ───────────────────────────────────────────────────────────────────────

switch ($Suite) {
    "help" {
        Get-Help $MyInvocation.MyCommand.Path -Detailed
        return
    }
    "python"   { Run-PythonTests }
    "go"       { Run-GoTests }
    "frontend" { Run-FrontendTests }
    "backend"  { Run-PythonTests; Run-GoTests }
    "database" { Run-PythonTests -TestPaths @("fetcher/tests/test_db.py") }
    "api"      { Run-GoTests -Packages @("api") }
    "all" {
        Run-PythonTests
        Run-GoTests
        Run-FrontendTests
    }
}

# ── Summary ────────────────────────────────────────────────────────────────────

$total = $script:totalPassed + $script:totalFailed + $script:totalSkipped

Write-Host ""
Write-Host ("=" * 70) -ForegroundColor Cyan
Write-Host "  TEST SUMMARY" -ForegroundColor Cyan
Write-Host ("=" * 70) -ForegroundColor Cyan
Write-Host ""

if ($script:suitesRun.Count -gt 0) {
    Write-Host "  Suites:  $($script:suitesRun -join ', ')" -ForegroundColor DarkGray
}
Write-Host "  Total:   $total tests" -ForegroundColor White

if ($script:totalPassed -gt 0) {
    Write-Host "  Passed:  $($script:totalPassed)" -ForegroundColor Green
}
if ($script:totalFailed -gt 0) {
    Write-Host "  Failed:  $($script:totalFailed)" -ForegroundColor Red
}
if ($script:totalSkipped -gt 0) {
    Write-Host "  Skipped: $($script:totalSkipped)" -ForegroundColor Yellow
}

if ($script:failedTests.Count -gt 0) {
    Write-Host ""
    Write-Host "  Failed tests:" -ForegroundColor Red
    foreach ($t in $script:failedTests) {
        Write-Host "    - $t" -ForegroundColor Red
    }
}

Write-Host ""
if ($script:totalFailed -eq 0 -and $total -gt 0) {
    Write-Host "  ALL TESTS PASSED" -ForegroundColor Green
} elseif ($script:totalFailed -gt 0) {
    Write-Host "  SOME TESTS FAILED" -ForegroundColor Red
    exit 1
} else {
    Write-Host "  NO TESTS RUN" -ForegroundColor Yellow
}
Write-Host ""
