#!/bin/sh
# install.sh — Install the latest git-stats release.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/dejo1307/git-stats/main/install.sh | sh
#
# Environment:
#   GIT_STATS_VERSION      install this version instead of the latest (e.g. 0.1.0)
#   GIT_STATS_INSTALL_DIR  install here instead of $HOME/.local/bin
#
# This script is deliberately POSIX sh, with no bashisms and no `set -o pipefail`.
# A piped script never gets to honour its own shebang — the interpreter on the
# left of the pipe runs it — and on Debian and Ubuntu /bin/sh is dash, which
# rejects `set -o pipefail` outright. A bash-only installer therefore dies before
# its first line of real work on the most common Linux systems. Keep it POSIX.

set -eu

REPO="dejo1307/git-stats"

fail() {
  echo "Error: $*" >&2
  exit 1
}

# --- Detect OS ---
OS="$(uname -s)"
case "$OS" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  # Git Bash, MSYS2 and Cygwin all report a decorated name; git-stats ships
  # Windows binaries, so install rather than refuse.
  MINGW*|MSYS*|CYGWIN*) OS=windows ;;
  *) fail "unsupported OS: $OS — see https://github.com/$REPO/releases for a manual download" ;;
esac

# --- Detect architecture ---
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)   ARCH=amd64 ;;
  arm64|aarch64)  ARCH=arm64 ;;
  *) fail "unsupported architecture: $ARCH — see https://github.com/$REPO/releases" ;;
esac

# --- Resolve version ---
VERSION="${GIT_STATS_VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' \
    | sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}//' -e 's/".*//')"
fi
[ -n "$VERSION" ] || fail "could not determine the latest version"
VERSION="${VERSION#v}"

BASE="git-stats-${VERSION}-${OS}-${ARCH}"
ASSET="${BASE}.tar.gz"
SHASUM="${BASE}.sha256"
DL="https://github.com/$REPO/releases/download/v${VERSION}"

echo "==> Downloading git-stats v${VERSION} for ${OS}/${ARCH} ..."

TMP="$(mktemp -d)"
# shellcheck disable=SC2064 # expand TMP now, while it is still set
trap "rm -rf \"$TMP\"" EXIT INT TERM

curl -fsSL -o "$TMP/$ASSET" "$DL/$ASSET" \
  || fail "no release asset $ASSET — check https://github.com/$REPO/releases"
curl -fsSL -o "$TMP/$SHASUM" "$DL/$SHASUM" \
  || fail "no checksum $SHASUM for $ASSET"

# --- Verify ---
# The checksum is fetched and checked, never skipped: a truncated or tampered
# download that still extracts is exactly what this catches. sha256sum is the
# GNU/Linux name, shasum the one macOS ships.
echo "==> Verifying checksum ..."
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$TMP" && sha256sum -c "$SHASUM") || fail "checksum verification failed"
elif command -v shasum >/dev/null 2>&1; then
  (cd "$TMP" && shasum -a 256 -c "$SHASUM") || fail "checksum verification failed"
else
  fail "neither sha256sum nor shasum found — cannot verify the download"
fi

echo "==> Extracting ..."
tar xzf "$TMP/$ASSET" -C "$TMP"

BIN="$BASE"
[ "$OS" = "windows" ] && BIN="${BIN}.exe"
[ -f "$TMP/$BIN" ] || fail "archive did not contain $BIN"

# --- Install ---
INSTALL_DIR="${GIT_STATS_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$INSTALL_DIR"

TARGET="git-stats"
[ "$OS" = "windows" ] && TARGET="git-stats.exe"

if ! install -m 755 "$TMP/$BIN" "$INSTALL_DIR/$TARGET" 2>/dev/null; then
  # BusyBox and some minimal images ship no install(1).
  cp "$TMP/$BIN" "$INSTALL_DIR/$TARGET" && chmod 755 "$INSTALL_DIR/$TARGET"
fi

echo "==> git-stats v${VERSION} installed to $INSTALL_DIR/$TARGET"

# --- Point the user at the next step ---
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo ""
    echo "$INSTALL_DIR is not in your PATH. Add it:"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

echo ""
echo "Next:"
echo "  git-stats version"
echo "  echo 'GIT_STATS_REPO=owner/name' > .env   # the repository to track"
echo "  git-stats collect"
