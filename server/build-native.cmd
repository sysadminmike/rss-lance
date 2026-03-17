@echo off
REM Build lancedb-go native library from Rust source (Windows / MSYS2 UCRT64)
REM Clones https://github.com/sysadminmike/lancedb-go to _lancedb-go/ on first run.
REM
REM You almost certainly do NOT need this.  Use pre-built libs from server\lib\.
REM Only rebuild if modifying the Rust/C FFI layer.
REM
REM Prerequisites (MSYS2 UCRT64 terminal):
REM   pacman -S mingw-w64-ucrt-x86_64-gcc mingw-w64-ucrt-x86_64-cmake
REM   pacman -S mingw-w64-ucrt-x86_64-nasm mingw-w64-ucrt-x86_64-make
REM   pacman -S mingw-w64-ucrt-x86_64-protobuf mingw-w64-ucrt-x86_64-rust

set PATH=C:\msys64\ucrt64\bin;C:\Program Files\Go\bin;%PATH%

REM Short target dir to avoid Windows MAX_PATH issues with aws-lc-sys
set CARGO_TARGET_DIR=C:\ct

set LANCE_DIR=%~dp0_lancedb-go
set LANCE_REPO=https://github.com/sysadminmike/lancedb-go.git
if exist "%LANCE_DIR%" (
    echo Updating lancedb-go repo...
    cd /d "%LANCE_DIR%"
    git pull --ff-only
) else (
    echo Cloning lancedb-go from %LANCE_REPO% ...
    git clone %LANCE_REPO% "%LANCE_DIR%"
)
cd /d "%LANCE_DIR%\rust"
echo Building lancedb-go native lib (x86_64-pc-windows-gnu)...
echo GCC:
gcc --version 2>&1 | findstr /N "." | findstr "^1:"
echo Cargo:
cargo --version
echo.
echo Target dir: %CARGO_TARGET_DIR% (short path to avoid MAX_PATH)
echo This takes ~20 minutes on first build...
echo.
cargo build --release
echo EXIT CODE: %ERRORLEVEL%
if %ERRORLEVEL% NEQ 0 (
    echo BUILD FAILED
    exit /b %ERRORLEVEL%
)
if exist "%CARGO_TARGET_DIR%\release\liblancedb_go.a" (
    echo SUCCESS: Library built
    if not exist "%~dp0lib\windows_amd64" mkdir "%~dp0lib\windows_amd64"
    copy /Y "%CARGO_TARGET_DIR%\release\liblancedb_go.a" "%~dp0lib\windows_amd64\liblancedb_go.a"
    echo Copied to lib\windows_amd64\
    if exist "%LANCE_DIR%\include\lancedb.h" (
        copy /Y "%LANCE_DIR%\include\lancedb.h" "%~dp0include\lancedb.h"
        echo Header synced to include\
    )
) else (
    echo FAILED: No library produced at %CARGO_TARGET_DIR%\release\liblancedb_go.a
    exit /b 1
)
