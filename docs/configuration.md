**English** | [한국어](configuration.ko.md) · [Back to README](../README.md)

# Installation and configuration

[Install](#install) · [Update](#update) · [Build](#build) · [Language](#language) · [Command defaults](#command-defaults) · [Shell completion](#shell-completion)

## Install

Install the latest release with the script. It reads the operating system and the architecture, checks the SHA-256, and installs the binary.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
```

The script installs `edc` in `/usr/local/bin` on Linux and in `~/.local/bin` on macOS.

On Linux, packet capture and some traces require root or the relevant capabilities. `trace arp` and `trace ndp` need no root. The `sudo` path includes `/usr/local/bin`, but it often excludes `~/.local/bin`.

If a non-root user cannot write there, the script uses `sudo`. If root cannot write there on Linux, the script uses `~/.local/bin`.

Set `BINDIR` to use another directory. Set `EDC_VERSION` to install an earlier version.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | BINDIR="$HOME/.local/bin" sh
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_VERSION=0.1.0 sh
```

Earlier versions of the script installed `edc` in `~/.local/bin` also on Linux. Ubuntu puts `~/.local/bin` before `/usr/local/bin` in `PATH`, so the shell runs that old copy. If the old copy is `edc`, the script removes it. The script checks the home of the user who runs it and, with `sudo`, the home of the user who ran `sudo`. Remove copies in other homes by hand, for example with `sudo rm /home/<user>/.local/bin/edc`.

If the install directory is not on `PATH`, the script finds your shell and prints the commands that add the directory. Set `EDC_MODIFY_PATH=1` to let the script add the line to the startup file of your shell, for example `~/.zshrc` or `~/.bashrc`. The script adds the line only once. The current shell does not get the new `PATH`. Open a new shell, or run the command that the script prints.

```bash
curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | EDC_MODIFY_PATH=1 sh
```

A release holds binaries for Linux and macOS on `amd64` and `arm64`.

## Update

`edc update` reads the latest release, checks the SHA-256, and replaces the running binary.

```bash
edc update           # confirm, then replace
edc update --check   # print the two versions only
edc update --yes     # skip the confirmation
```

`edc` writes the new file next to the old one and renames it. A failed download leaves the earlier binary in place. If you cannot write to the directory, `edc` uses `sudo` to copy and rename the new file, as the install script does. `edc` downloads and checks the file as your user. The confirmation shows `privilege sudo`, and `sudo` asks for your password once if it needs one. If `sudo` is not available or fails, `edc` stops with exit code `3` before it downloads anything.

## Build

`edc` needs Go 1.25 or later. The live screens use these dependencies.

- `charm.land/bubbletea/v2`
- `charm.land/bubbles/v2`
- `charm.land/lipgloss/v2`

```bash
make build VERSION=0.1.0-dev
./bin/edc version
```

Install `edc` in `~/.local/bin`.

```bash
make install VERSION=0.1.0-dev
~/.local/bin/edc version
```

Set `PREFIX` or `BINDIR` to use another install path.

```bash
make install PREFIX=/usr/local
```

## Language

`edc` prints English by default. It also carries Korean and Japanese.

Set the language in the config file. On Linux and macOS, `edc` reads `$XDG_CONFIG_HOME/edc/config.toml` when set, or `~/.config/edc/config.toml` by default.

```toml
lang = "ko"
```

If `config.toml` is absent, Linux still reads `~/.config/edc/config.yaml`, and macOS still reads `~/Library/Application Support/edc/config.yaml`. `edc setup` saves a new TOML file and leaves the YAML file intact.

Set `EDC_LANG` to change the language for one run. It wins over the config file.

```bash
EDC_LANG=ja edc where
```

`edc` accepts `en`, `ko`, and `ja`. It reads a locale name such as `ko_KR.UTF-8` and keeps the language part. An unknown value falls back to English, and so does a message that a language misses.

## Command defaults

The same config file can hold repeat-safe command defaults. Precedence is built-in default, config, then an explicit CLI option. Invalid keys, types, or ranges stop the command with exit code `2` instead of being ignored.

Run `edc setup` in a terminal to create or update the file. The wizard configures one section at a time, keeps existing values on Enter, removes an optional value with `!clear`, previews the complete config, and asks before an atomic mode `0600` save. The config directory is mode `0700`; cancel returns exit code `4`.

The following TOML example applies to both Linux and macOS.

```toml
lang = "en"

[defaults.common]
timeout = "15s"
verbose = false
redact = false

[defaults.doctor]
profile = "default"

[defaults.tls]
min_days = 14

[defaults.capture]
duration = "15s"
count = 500

[defaults.remote]
connect_timeout = "10s"
output_limit = 65536
parallel = 0

[defaults.du]
workers = 4

[defaults.history]
limit = 200

[defaults.changes]
since = "24h"

[defaults.snapshot]
since = "6h"

[defaults.events]
since = "6h"

[defaults.update]
timeout = "60s"

[defaults.quality]
timeout = "30s"
server = ""

[defaults.log]
stream = "both"
output = ""
command_display = "full"
max_size_mb = 10
keep_files = 3
restart = "never"
max_restarts = 3
restart_delay = "5s"
timeout = "0s"
kill_after = "5s"
history_limit = 20
```

Command-specific values override `defaults.common`. Positional targets, URLs, hosts, and remote groups are never stored, nor are action options such as `yes`, `force`, `dry-run`, `list`, and `check`. Persisted remote inventory and recipe paths must be absolute. Empty path values disable that default.

The setup wizard recommends automatic files for `edc log`.

An empty output path selects one file per run. Custom output paths require an existing parent directory.

The command also creates the parent for the legacy recommended `edc.log` path.

<a id="aliases"></a>

## Shell completion

`edc shell` prints completion and short aliases together. Without a shell name, it uses `$SHELL`. Apply it to the current session:

```sh
source <(edc shell)
```

For future sessions, add **only the line for your shell** at the end of its startup file:

```sh
# ~/.zshrc
source <(edc shell zsh)

# ~/.bashrc
source <(edc shell bash)
```

edc does not edit startup files. Running `source` directly applies to the current session; adding the line to the startup file applies it in each new shell. Remove that line to disable it for future sessions. With zsh `ZDOTDIR`, use `.zshrc` in that directory.

For completion only, use `source <(edc shell zsh --no-alias)`. Completion suggests commands, options, and inventory group names. If zsh completion is not initialized, the script initializes it without saving a cache file.

Aliases include `eg` for `edc grep`, `ev` for `edc view`, and `etop` for `edc top`. The script preserves existing aliases, functions, and commands and reports skipped names. There are no aliases for `route`, `disk`, or `change`, to avoid accidental host changes. Later options override earlier ones: `ege --last 50 app.log` prints 50 errors. zsh completes edc options through aliases.

The existing `edc alias [zsh|bash] [--plain]` and `edc completion <zsh|bash>` commands remain supported. `--plain` prints bare alias lines for editing. The scripts still use `edc completion groups` internally.
