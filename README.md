# atlas9

TUI for the [Atlas](https://atlasgo.io/) CLI: environment selection, stage workflow (Status → Diff → Lint → Dry-Run → Apply), syntax-highlighted output, and Docker status.

## Quick Start

Use homebrew to install:
```
brew install sio2boss/tap/atlas9
```

Run from a directory containing an `atlas.hcl` file:

```bash
atlas9
```

Or with a specific environment:

```bash
atlas9 --env prod
```

## Requirements

- **Go** 1.22+
- **Atlas** CLI on `PATH` ([install](https://atlasgo.io/getting-started#installation))
- **atlas.hcl** in your project directory
- **Docker** (optional but recommended; status shown in header)


## Usage

### Command-line options

```
atlas9 [options]

Options:
  -h, --help          Show help
  -v, --version       Show version
  -e, --env <env>     Set initial environment (local, prod) [default: local]
```

### Stages

1. **Status** — Show current migration status
2. **Diff** — Generate migration files from schema changes
3. **Lint** — Lint migrations (requires Atlas Cloud login for full features)
4. **Dry-Run** — Preview changes without applying
5. **Apply** — Apply migrations (shows confirmation dialog)


### Keys

| Key | Action |
|-----|--------|
| **Tab** / **Shift+Tab** | Cycle through stages |
| **↓ / ↑** | Scroll output |
| **Enter** | Run current stage |
| **i** | Edit command (vim-like: Esc to exit) |
| **e** | Select environment (local / prod) |
| **c** | Edit `atlas.hcl` config (Esc save & exit, Ctrl+C cancel) |
| **h** | Help |
| **q** | Quit |


### Edit mode

Press **i** to enter edit mode and modify the command. The command line will be underlined. Press **Esc** or **Ctrl+C** to exit edit mode, or **Enter** to run the edited command.


### Configuration

Create an `atlas.hcl` file in your project:

```hcl
env "local" {
  src = "file://schema.sql"
  url = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
  dev = "docker://postgres/15"
}

env "prod" {
  src = "file://schema.sql"
  url = env("DATABASE_URL_PROD")
}
```

Press **c** to edit this file from within atlas9.


## Development

### From source

```bash
make all
cp ./atlas9 ~/.local/bin/
```

The binary will be created at `./atlas9` and we copied it to `~/.local/bin/`.  Make sure that directory is on your $PATH.

### Cross-platform release builds

```bash
make release
```

Binaries for Linux, macOS, Windows, FreeBSD, OpenBSD, and NetBSD will be in the `release/` directory.
