#!/bin/sh
# edc installer.
#
#   curl -fsSL https://raw.githubusercontent.com/x-mesh/edc/main/install.sh | sh
#
# Environment:
#   EDC_VERSION      version to install, without the leading v (default: latest)
#   BINDIR           install directory (default: $HOME/.local/bin)
#   EDC_MODIFY_PATH  set to 1 to add BINDIR to PATH in the startup file of your shell
#
# The script downloads the release asset, checks its SHA-256 against
# checksums.txt, and then installs the binary.

set -eu

REPO="x-mesh/edc"
BINDIR="${BINDIR:-$HOME/.local/bin}"
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
		sha256sum --check --quiet expected.txt
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 --check --status expected.txt
	else
		fail "sha256sum or shasum is required"
	fi
) || fail "checksum does not match for $asset"

tar -xzf "$tmp/$asset" -C "$tmp" edc || fail "cannot extract edc from $asset"

mkdir -p "$BINDIR" || fail "cannot create $BINDIR"
install_path="$BINDIR/edc"
# Write next to the target and rename, so a running edc keeps working.
cp "$tmp/edc" "$install_path.new" || fail "cannot write to $BINDIR"
chmod 0755 "$install_path.new"
mv "$install_path.new" "$install_path" || fail "cannot replace $install_path"

echo "installed $install_path"
"$install_path" version

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
		# macOS Terminal starts bash as a login shell, which reads ~/.bash_profile and not ~/.bashrc.
		if [ "$os" = darwin ]; then
			echo "$HOME/.bash_profile"
		else
			echo "$HOME/.bashrc"
		fi
		;;
	fish) echo "${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish" ;;
	tcsh) echo "$HOME/.tcshrc" ;;
	csh) echo "$HOME/.cshrc" ;;
	*) echo "$HOME/.profile" ;;
	esac
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
	;;
esac
