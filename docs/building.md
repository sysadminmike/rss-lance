# Building RSS-Lance

## Prerequisites

| Tool | Version | Install |
|---|---|---|
| **Go** | 1.21+ | [go.dev/dl](https://go.dev/dl/) or `winget install GoLang.Go` / `brew install go` |
| **Python** | 3.10+ | [python.org](https://python.org) or `winget install Python.Python.3.12` / `brew install python@3.12` |
| **Git** | any | `winget install Git.Git` (Windows) / `apt install git` (Linux) / `xcode-select --install` (macOS) |

### Windows Build Dependencies (GCC / MSYS2)

The Go server links against a pre-built native library (`liblancedb_go.a`) via CGo, which requires **GCC** at compile time. **End users do not need GCC at runtime** - it's only needed to compile the Go binary from source.

If GCC is not on your PATH, the build script automatically checks common MSYS2 install locations (`C:\msys64\ucrt64\bin`, `C:\msys64\mingw64\bin`). If not found, it prints installation instructions and exits.

**Quick GCC setup (one-time):**

1. Download & install [MSYS2](https://www.msys2.org/)
2. Open the **MSYS2 UCRT64** terminal and run:
   ```bash
   pacman -S mingw-w64-ucrt-x86_64-gcc
   ```
3. Either add `C:\msys64\ucrt64\bin` to your system PATH, or let `build.ps1` find it automatically

> **Pre-built binaries:** If pre-built `rss-lance-server.exe` binaries are available from the project releases, you can skip all build steps entirely - just download the binary and run it. No Go, no GCC, no compilation needed.

---

## Build Scripts

Both build scripts accept an optional directory flag so you can run the build from anywhere:

| OS | Syntax |
|---|---|
| **Windows** | `.\build.ps1 [-Dir <path>] <command>` |
| **Linux/macOS** | `./build.sh [-d <path>] <command>` |

If omitted, the scripts default to their own directory.

### Commands

| Command | Description |
|---|---|
| `setup` | Create Python venv, install all deps, verify Go |
| `server` | Build Go server for current platform |
| `server-all` | Cross-compile for Windows/Linux/macOS/FreeBSD (amd64+arm64) |
| `fetcher` | Install Python fetcher dependencies only |
| `run-fetcher` | Run the feed fetcher daemon |
| `fetch-once` | Fetch articles once and exit |
| `run-server` | Start the HTTP server |
| `demo-data` | Insert demo RSS feeds into LanceDB |
| `duckdb` | Download DuckDB CLI binary into `tools/` |
| `migrate` | Install migration dependencies (see [importing](importing.md) for TT-RSS, FreshRSS, Miniflux, OPML) |
| `migrate-cleanup` | Remove migrate scripts and their deps |
| `clean` | Remove `build/` directory |
| `test` | Run all test suites (delegates to `test.ps1` / `test.sh`) |
| `all` | Full build: setup + duckdb + server + demo-data + tests |
| `minimum` | Bare minimum to run the app: setup + duckdb + server. No tests, no demo data, no Node.js needed |
| `help` | Show available commands |

### Flags

| Flag | Platform | Effect |
|---|---|---|
| `-NoTests` | Windows | Skip tests when running `all`: `.\build.ps1 -NoTests all` |
| `--no-tests` | Linux/macOS | Skip tests when running `all`: `./build.sh --no-tests all` |

Tests are **enabled by default** in the `all` command.

### Minimal Build

If you just want to get the app running with no extras, use `minimum`:

```powershell
# Windows
.\build.ps1 minimum
```

```bash
# Linux / macOS
./build.sh minimum
```

This runs only **setup → duckdb → server** — no tests, no demo data, no Node.js required. You can always add demo feeds later with `run.ps1 demo-data` / `run.sh demo-data`.

### Binary Package Fallback

The build scripts install Python packages using a **binary-first, source fallback** strategy:

1. First attempts a bulk `pip install -r requirements.txt`
2. If that fails, retries each package individually
3. For any package with `-binary` in its name (e.g. `psycopg2-binary`) that fails to install, automatically retries with the `-binary` suffix stripped (e.g. `psycopg2`), which compiles from source

This means pre-built wheels are preferred for speed, but the build still succeeds on platforms where binary wheels aren't available (e.g. Alpine Linux, older Python versions, exotic architectures).

> **Note:** `psycopg2-binary` is only in `migrate/requirements.txt` — it is **not** installed during normal builds (`setup`, `fetcher`, `minimum`, `all`). It is only pulled in when you explicitly run `build.ps1 migrate` / `build.sh migrate` for TT-RSS PostgreSQL migration.

---

## Custom Installation Directory

By default, the build scripts create all artifacts (Python venv, server binary, frontend files, configuration) in the same directory as the build script itself. If you want to install RSS-Lance in a separate location (for better organization, system-wide installation, or deployment), use the `-Dir` (Windows) or `-d` (Linux/macOS) flag.

### What Happens With a Custom Directory

When you specify a custom install directory:

1. All build artifacts are placed in the specified directory
2. The directory becomes **self-contained** — you can move or copy it to another location and run it independently
3. The original cloned git repository is no longer needed and can be deleted to free up space
4. All subsequent run commands must be executed from the install directory or you must specify the directory again

### Installation Examples

#### Windows

```powershell
# Build and install to C:\Apps\rss-lance
.\build.ps1 -Dir C:\Apps\rss-lance all

# Then navigate to the install directory and run from there
cd C:\Apps\rss-lance
.\run.ps1 server

# Or run commands from anywhere by specifying -Dir again
.\build.ps1 -Dir C:\Apps\rss-lance help
```

#### Linux / macOS

```bash
# Build and install to /opt/rss-lance
./build.sh -d /opt/rss-lance all

# Then navigate to the install directory and run from there
cd /opt/rss-lance
./run.sh server

# Or run commands from anywhere by specifying -d again
./build.sh -d /opt/rss-lance help
```

### Cleanup: Deleting the Repository Clone

Once installation is complete in your custom directory, the original cloned repository is no longer needed:

```powershell
# Windows - delete the cloned repo (NOT the install directory)
Remove-Item -Recurse -Force C:\path\to\rss-lance-clone

# Linux / macOS - delete the cloned repo (NOT the install directory)
rm -rf /path/to/rss-lance-clone
```

The install directory at `C:\Apps\rss-lance` (or `/opt/rss-lance`, etc.) contains everything needed to run RSS-Lance independently.

### Moving an Installation

Since the install directory is self-contained, you can move it to a different location at any time. Just use standard file operations (`cp -r`, `rsync`, Windows Explorer, etc.):

```bash
# Linux / macOS example: move installation to a new location
cp -r /opt/rss-lance /home/user/rss-lance-backup
cd /home/user/rss-lance-backup
./run.sh server
```

---

## Step-by-step: Windows

> **PowerShell execution policy:** If running scripts is disabled, prefix commands with:
> ```powershell
> powershell -ExecutionPolicy Bypass -File .\build.ps1 <command>
> ```

```powershell
# 1. First-time setup - creates .venv, installs Python deps, checks Go
.\build.ps1 setup

# 2. Download DuckDB CLI into tools/ (required on Windows)
.\build.ps1 duckdb

# 3. Build the Go server binary → build\rss-lance-server.exe
.\build.ps1 server

# 4. Insert demo RSS feeds
.\run.ps1 demo-data

# 5. Fetch articles for all feeds
.\run.ps1 fetch-once

# 6. Start the HTTP server
.\run.ps1 server
# → http://127.0.0.1:8080
```

## Step-by-step: Linux

```bash
# 1. First-time setup
./build.sh setup

# 2. Download DuckDB CLI (optional on Linux - embedded CGo is used by default)
./build.sh duckdb

# 3. Build the Go server
./build.sh server

# 4. Insert demo RSS feeds
./run.sh demo-data

# 5. Fetch articles
./run.sh fetch-once

# 6. Start the HTTP server
./run.sh server
# → http://127.0.0.1:8080
```

## Step-by-step: macOS

```bash
# Install prerequisites (if not already installed)
brew install go python@3.12 git

# Then follow the same steps as Linux above
./build.sh setup
./build.sh duckdb
./build.sh server
./run.sh demo-data
./run.sh fetch-once
./run.sh server
```

> **Apple Silicon (M1/M2/M3/M4):** Fully supported. The Go server builds natively for arm64, and DuckDB downloads a universal binary.

---

## Building the Native Library from Source

> **You almost certainly do NOT need to do this.** The pre-built `liblancedb_go.a` is checked into `server/lib/windows_amd64/`. Only rebuild if you are modifying the Rust/C FFI layer.

### Prerequisites (MSYS2 UCRT64)

These packages must be installed manually in the **MSYS2 UCRT64** terminal (not PowerShell,
not the regular MSYS2 MSYS shell). This step cannot be automated from VS Code or PowerShell
because MSYS2 runs in its own shell environment.

1. Install [MSYS2](https://www.msys2.org/) if not already installed (default path: `C:\msys64`)
2. Open **MSYS2 UCRT64** from the Start Menu - look for the icon with a yellow/orange stripe.
   Do **not** use the purple "MSYS2 MSYS" or blue "MSYS2 MINGW64" variants - those target
   different toolchains and the build will fail.
3. Run the following commands (confirm each prompt with `Y`):

```bash
pacman -S mingw-w64-ucrt-x86_64-gcc
pacman -S mingw-w64-ucrt-x86_64-cmake
pacman -S mingw-w64-ucrt-x86_64-nasm
pacman -S mingw-w64-ucrt-x86_64-make
pacman -S mingw-w64-ucrt-x86_64-protobuf
pacman -S mingw-w64-ucrt-x86_64-rust
```

4. Close the MSYS2 terminal when done. The tools install to `C:\msys64\ucrt64\bin` and
   `build-native.ps1` will find them automatically.

### Build

```powershell
cd server
.\build-native.ps1
```

The script:
1. Sets `CARGO_TARGET_DIR=C:\ct` (short path - avoids Windows MAX_PATH issues with `aws-lc-sys`)
2. Runs `cargo build --release` in `server/lancedb-go/rust/`
3. Copies the built `liblancedb_go.a` (~350 MB) to `server/lib/windows_amd64/`

**First build takes ~20 minutes.** Subsequent incremental builds are much faster.

### Linux / macOS

```bash
cd server/lancedb-go/rust
cargo build --release
cp target/release/liblancedb_go.a ../lib/linux_amd64/  # adjust for your arch
```

### Troubleshooting

| Problem | Fix |
|---|---|
| Path too long / `aws-lc-sys` errors | `CARGO_TARGET_DIR` is already set to `C:\ct` by the script |
| SSL certificate / git fetch errors | Add `[net]\ngit-fetch-with-cli = true` to `~/.cargo/config.toml` |
| Missing `cmake` / `nasm` / `protoc` | Install via MSYS2 `pacman` (see prerequisites above) |
| `ar: malformed archive` or `no index` | Rebuild with GNU target (the script does this) |
| Linker: `undefined reference to NtCreateFile` | Add `-lntdll` to CGO_LDFLAGS (build.ps1 already includes this) |
| `libwinpthread-1.dll was not found` at runtime | Rebuild with `build.ps1 server` (adds `-static` automatically) |
