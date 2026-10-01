#!/bin/sh
# edc installer.
#
#   curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
#
# Environment:
#   EDC_VERSION      version to install, without the leading v (default: latest)
#   BINDIR           install directory (default: /usr/local/bin on Linux, $HOME/.local/bin on macOS)
#   EDC_MODIFY_PATH  set to 1 to add BINDIR to PATH in the startup file of your shell
#
# The script downloads the release asset, checks its SHA-256 against
# checksums.txt, and then installs the binary. If the user cannot write to
# BINDIR, the script uses sudo only to copy the binary.

set -eu

REPO="x-mesh/edc"
VERSION="${EDC_VERSION:-latest}"

fail() {
	echo "install.sh: $1" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

# download writes a URL to a file with curl or wget.
download() {
	url="$1"
	out="$2"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$url" -o "$out" || fail "cannot download $url"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$out" "$url" || fail "cannot download $url"
	else
		fail "curl or wget is required"
	fi
}

need tar
need mkdir
need uname

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
linux | darwin) ;;
*) fail "unsupported operating system: $os" ;;
esac

# On Linux, trace and capture need root. sudo uses secure_path, which does not include ~/.local/bin, so
# edc goes where sudo and every user find the same binary.
bindir_is_default=0
if [ -z "${BINDIR:-}" ]; then
	bindir_is_default=1
	case "$os" in
	linux) BINDIR=/usr/local/bin ;;
	*) BINDIR="$HOME/.local/bin" ;;
	esac
fi

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch="amd64" ;;
arm64 | aarch64) arch="arm64" ;;
*) fail "unsupported architecture: $arch" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

if [ "$VERSION" = "latest" ]; then
	download "https://api.github.com/repos/$REPO/releases/latest" "$tmp/release.json"
	VERSION=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' "$tmp/release.json" | head -1)
	[ -n "$VERSION" ] || fail "cannot read the latest version from the GitHub API"
fi

asset="edc_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v${VERSION}"

echo "edc ${VERSION} for ${os}/${arch}"

download "$base/$asset" "$tmp/$asset"
download "$base/checksums.txt" "$tmp/checksums.txt"

# Keep only the line of this asset, so an absent entry fails the check.
awk -v name="$asset" '$2 == name || $2 == "*" name' "$tmp/checksums.txt" > "$tmp/expected.txt"
[ -s "$tmp/expected.txt" ] || fail "checksums.txt has no entry for $asset"

(
	cd "$tmp"
	if command -v sha256sum >/dev/null 2>&1; then
		# BusyBox sha256sum on Alpine takes only the short options. -c prints "OK" lines on stdout.
		sha256sum -c expected.txt >/dev/null
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 --check --status expected.txt
	else
		fail "sha256sum or shasum is required"
	fi
) || fail "checksum does not match for $asset"

tar -xzf "$tmp/$asset" -C "$tmp" edc || fail "cannot extract edc from $asset"

# writable_dir tells whether the user can create files in a directory, or in its nearest parent that
# exists when the directory does not exist yet.
writable_dir() {
	dir=$1
	while [ ! -d "$dir" ]; do
		dir=$(dirname "$dir")
	done
	probe=$(mktemp "$dir/.edc-write-test.XXXXXX" 2>/dev/null) || return 1
	rm -f "$probe" || return 1
}

elevate=""
if ! writable_dir "$BINDIR"; then
	if [ "$(id -u)" = 0 ]; then
		if [ "$os" = linux ] && [ "$bindir_is_default" = 1 ] && [ -n "${HOME:-}" ] && writable_dir "$HOME/.local/bin"; then
			echo "cannot write to $BINDIR; installing to $HOME/.local/bin instead"
			BINDIR="$HOME/.local/bin"
		else
			fail "cannot write to $BINDIR"
		fi
	else
		command -v sudo >/dev/null 2>&1 || fail "cannot write to $BINDIR. Run the installer as root, or set BINDIR=\$HOME/.local/bin"
		echo "installing to $BINDIR with sudo"
		# Check sudo with a command, not with sudo -v. By default sudo -v asks for a password unless every sudoers rule of
		# the user has NOPASSWD, so it fails for the Ubuntu cloud user, whose NOPASSWD rule comes after "%sudo ALL=(ALL:ALL) ALL".
		# If a rule needs a password, sudo asks once from the terminal, also with curl | sh, and the copy steps reuse it.
		sudo true || fail "sudo failed. Run the installer as root, or set BINDIR=\$HOME/.local/bin"
		elevate="sudo"
	fi
fi

$elevate mkdir -p "$BINDIR" || fail "cannot create $BINDIR"
install_path="$BINDIR/edc"
# Write next to the target and rename, so a running edc keeps working.
$elevate cp "$tmp/edc" "$install_path.new" || fail "cannot write to $BINDIR"
$elevate chmod 0755 "$install_path.new"
$elevate mv "$install_path.new" "$install_path" || fail "cannot replace $install_path"

echo "installed $install_path"
"$install_path" version

# remove_old_copy removes an edc that an earlier installer put in ~/.local/bin. Ubuntu puts ~/.local/bin
# before /usr/local/bin in PATH, so the old binary would still run. A file that is not edc stays.
removed=""
remove_old_copy() {
	old="$1/.local/bin/edc"
	[ "$old" != "$install_path" ] && [ -f "$old" ] || return 0
	old_version=$("$old" version 2>/dev/null | head -1) || true
	case "$old_version" in
	"edc "*) ;;
	*)
		echo "found $old, but it is not edc. Remove it by hand if you do not use it."
		return 0
		;;
	esac
	if rm -f "$old"; then
		echo "removed old $old (${old_version% (*})"
		removed=1
	else
		echo "cannot remove old $old. Remove it by hand: rm $old"
	fi
}

[ -z "${HOME:-}" ] || remove_old_copy "$HOME"
# With sudo, HOME is /root, but the old copy is usually in the home of the user who ran sudo.
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ] && command -v getent >/dev/null 2>&1; then
	sudo_home=$(getent passwd "$SUDO_USER" | cut -d: -f6)
	[ -z "$sudo_home" ] || [ "$sudo_home" = "${HOME:-}" ] || remove_old_copy "$sudo_home"
fi
if [ -n "$removed" ]; then
	echo "If your shell still runs the old path, run: hash -r"
fi

found=$(command -v edc 2>/dev/null || true)
if [ -n "$found" ] && [ "$found" != "$install_path" ]; then
	echo "another edc comes first on PATH: $found"
fi

# user_shell names the shell that started the installer. With curl | sh the parent process is that
# shell. A parent sh is usually a wrapper such as sh -c or a Dockerfile RUN, and sudo is not a shell,
# so the login shell in $SHELL is the next guess.
user_shell() {
	parent=$(cat "/proc/$PPID/comm" 2>/dev/null || ps -o comm= -p "$PPID" 2>/dev/null || true)
	parent=${parent##*/}
	parent=${parent#-}
	case "$parent" in
	bash | zsh | fish | ksh | mksh | tcsh | csh)
		echo "$parent"
		return
		;;
	esac
	login=${SHELL:-sh}
	echo "${login##*/}"
}

# startup_file names the file that the shell reads when a terminal opens.
startup_file() {
	case "$1" in
	zsh) echo "${ZDOTDIR:-$HOME}/.zshrc" ;;
	bash)
		if [ "$os" != darwin ]; then
			echo "$HOME/.bashrc"
			return
		fi
		# macOS Terminal starts bash as a login shell. It reads the first of these files that exists, so
		# a new ~/.bash_profile would hide an existing ~/.bash_login or ~/.profile.
		for login in "$HOME/.bash_profile" "$HOME/.bash_login" "$HOME/.profile"; do
			if [ -e "$login" ]; then
				echo "$login"
				return
			fi
		done
		echo "$HOME/.bash_profile"
		;;
	fish) echo "${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish" ;;
	tcsh) echo "$HOME/.tcshrc" ;;
	csh) echo "$HOME/.cshrc" ;;
	*) echo "$HOME/.profile" ;;
	esac
}

# print_path_hint tells the user how to add BINDIR to PATH, or adds it when EDC_MODIFY_PATH=1.
print_path_hint() {
	if grep -qsxF "$line" "$file"; then
		echo "$file already adds $BINDIR to PATH. Open a new $shell or run:"
		echo "  $line"
	elif [ "${EDC_MODIFY_PATH:-}" = 1 ]; then
		mkdir -p "$(dirname "$file")" || fail "cannot create the directory of $file"
		printf '\n# added by the edc installer\n%s\n' "$line" >>"$file" || fail "cannot write $file"
		echo "added $BINDIR to PATH in $file. Open a new $shell or run:"
		echo "  $line"
	else
		echo "edc is not on PATH. To add $BINDIR for $shell, run:"
		echo "  echo '$line' >> \"$file\""
		echo "  $line"
		echo "Set EDC_MODIFY_PATH=1 to let the installer add it."
	fi
}

case ":$PATH:" in
*":$BINDIR:"*) ;;
*)
	shell=$(user_shell)
	file=$(startup_file "$shell")
	case "$shell" in
	fish) line="set -gx PATH \"$BINDIR\" \$PATH" ;;
	tcsh | csh) line="setenv PATH \"$BINDIR:\$PATH\"" ;;
	*) line="export PATH=\"$BINDIR:\$PATH\"" ;;
	esac
	case "$BINDIR$file" in
	# The line goes inside double quotes and the command inside single quotes, so these characters
	# would write a broken startup file.
	*[\"\'\$\`\\]*)
		# printf keeps a backslash in the path, and the echo of dash reads it as an escape.
		printf 'edc is not on PATH. Add %s to PATH in %s by hand.\n' "$BINDIR" "$file"
		;;
	*)
		print_path_hint
		;;
	esac
	;;
esac
