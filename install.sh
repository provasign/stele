#!/usr/bin/env bash
#
# Install Stele from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/provasign/stele/main/install.sh | bash
#
# Environment variables (all optional):
#   VERSION        release tag to install   (default: latest)
#   INSTALL_DIR    install directory         (default: $HOME/bin)
#   PROJECT        a repository to set up afterwards with `stele init`
#
# Supported platforms: linux-amd64, linux-arm64, darwin-amd64, darwin-arm64.
# Stele needs the Z3 solver on PATH; this script says how to install it.
set -euo pipefail

PRODUCT="stele"
REPO="provasign/stele"
INSTALL_DIR="${INSTALL_DIR:-$HOME/bin}"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m✅\033[0m %s\n' "$*"; }
err()  { printf '\033[1;31m❌\033[0m %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "required tool not found: $1"; }

need curl

# ── Platform detection ───────────────────────────────────────────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)        ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "unsupported architecture: $ARCH" ;;
esac
case "$OS" in
  linux|darwin) ;;
  *) die "unsupported OS: $OS" ;;
esac
info "Platform: ${OS}-${ARCH}"

# ── Version resolution ───────────────────────────────────────────────────────
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  info "Resolving latest release…"
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')"
  [ -n "$VERSION" ] || die "could not determine latest version — set VERSION=vX.Y.Z"
fi
info "Version: ${VERSION}"

FILE="${PRODUCT}-${VERSION}-${OS}-${ARCH}"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ── Download and verify ──────────────────────────────────────────────────────
info "Downloading ${FILE}…"
curl -fSL --progress-bar "${BASE}/${FILE}" -o "${TMP}/${FILE}" \
  || die "download failed — check https://github.com/${REPO}/releases"
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}
ACTUAL="$(sha256 "${TMP}/${FILE}")"
curl -fsSL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt" \
  || die "could not download checksums.txt; refusing an unverified install"
EXPECTED="$(grep -E "  (\./)?${FILE}\$" "${TMP}/checksums.txt" | awk '{print $1}' | head -1)"
[ -n "$EXPECTED" ] || die "checksums.txt has no entry for ${FILE}"
[ "$EXPECTED" = "$ACTUAL" ] \
  || die "CHECKSUM MISMATCH — refusing to install (expected $EXPECTED, got $ACTUAL)"
ok "${FILE}: checksum verified"

# ── Install ──────────────────────────────────────────────────────────────────
# A running `stele serve` keeps the old binary; stop it so the next check
# starts the new one (stele check falls back to a one-shot check meanwhile).
if command -v pkill >/dev/null 2>&1; then
  pkill -f "${PRODUCT} serve" 2>/dev/null && info "Stopped a running stele serve" || true
fi
mkdir -p "$INSTALL_DIR"
mv "${TMP}/${FILE}" "${INSTALL_DIR}/${PRODUCT}"
chmod +x "${INSTALL_DIR}/${PRODUCT}"
[ "$OS" = "darwin" ] && xattr -d com.apple.quarantine "${INSTALL_DIR}/${PRODUCT}" 2>/dev/null || true
[ "$OS" = "darwin" ] && codesign -f -s - "${INSTALL_DIR}/${PRODUCT}" 2>/dev/null || true
INSTALLED_VERSION="$("${INSTALL_DIR}/${PRODUCT}" version 2>/dev/null || true)"
[ "$INSTALLED_VERSION" = "stele ${VERSION}" ] \
  || die "installed binary verification failed: expected 'stele ${VERSION}', got '${INSTALLED_VERSION:-no output}'"
ok "${PRODUCT} ${VERSION} → ${INSTALL_DIR}/${PRODUCT}"

# ── PATH ─────────────────────────────────────────────────────────────────────
SHELL_RC="$HOME/.zshrc"; [ -n "${BASH_VERSION:-}" ] && [ "$OS" = "linux" ] && SHELL_RC="$HOME/.bashrc"
LINE="export PATH=\"${INSTALL_DIR}:\$PATH\""
case ":$PATH:" in
  *":${INSTALL_DIR}:"*) ;;
  *) grep -qsF "$LINE" "$SHELL_RC" 2>/dev/null || { echo "$LINE" >> "$SHELL_RC" && info "Added ${INSTALL_DIR} to PATH in ${SHELL_RC}"; } ;;
esac
export PATH="${INSTALL_DIR}:$PATH"

# ── Z3 ───────────────────────────────────────────────────────────────────────
if command -v z3 >/dev/null 2>&1; then
  ok "z3 found: $(z3 --version)"
else
  Z3_CMD="brew install z3"
  if [ "$OS" = "linux" ]; then
    for pm in "apt-get:sudo apt-get install -y z3" "dnf:sudo dnf install -y z3" "pacman:sudo pacman -S --noconfirm z3" "apk:sudo apk add z3"; do
      command -v "${pm%%:*}" >/dev/null 2>&1 && { Z3_CMD="${pm#*:}"; break; }
    done
  fi
  err "Stele needs the Z3 solver, which is not on PATH. Install it with:  ${Z3_CMD}"
  NEED_Z3="$Z3_CMD"
fi

# ── Optional project setup ───────────────────────────────────────────────────
if [ -n "${PROJECT:-}" ]; then
  [ -d "$PROJECT" ] || die "project dir not found: $PROJECT"
  ( cd "$PROJECT" && "${INSTALL_DIR}/${PRODUCT}" init --yes ) \
    && ok "stele: ${PROJECT} set up" || err "stele init failed — run it in the project: stele init"
fi

# ── What it is for, and the next steps ───────────────────────────────────────
n=0; step() { n=$((n+1)); printf '  %d. %s\n' "$n" "$1"; }
printf '\n%s %s installed: requirements that hold.\n' "$PRODUCT" "$VERSION"
printf 'Stele checks each new request or spec change against every approved requirement,\nand asks you about any contradiction before code is written.\n\nNext steps:\n'
if [ -n "${NEED_Z3:-}" ]; then step "Install the Z3 solver:  ${NEED_Z3}"; fi
step "cd your-repo"
step "stele init       finds your specs (or the requests your agents declare with Shale)"
printf '                      and your coding agents; writes their instructions and hooks\n'
step "Commit what it writes, so every clone's agent works the same way"
step "Open your coding agent and work as usual. On a project with specs, it offers"
printf '     to check them all once, and asks you only about contradictions.\n'
