#!/usr/bin/env bash
# install.sh — one-command installer for aidev.
#
# Works in two modes:
#
#   1. Download mode (default when Go is not installed):
#      Downloads a prebuilt binary from the latest GitHub release for
#      the detected OS/arch. Requires only `curl` and `tar`.
#
#   2. Build mode (default when Go >= 1.24 is installed):
#      Builds the binary from the local repo checkout. Used by
#      developers and CI.
#
# Force a specific mode with --from-release or --from-source.
#
# Usage (download mode — no Go required):
#   curl -sSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh | bash
#
# Usage (build mode — from a cloned repo):
#   git clone https://github.com/AiSU-AI/aidev.git
#   cd aidev
#   ./install.sh
#
# Flags:
#   --bin <dir>       target bin directory (default: ~/.local/bin)
#   --from-release    force download mode (error if GH release missing)
#   --from-source     force build mode (error if Go missing)
#   --version <ver>   specific release tag to download (default: latest)
#   --force           overwrite existing binary / config / plugin files
#   --no-doctor       skip the final `aidev doctor` smoke test
#   --no-init         skip the interactive `aidev init` walkthrough at the end
#   --no-prereqs      skip the "suggest installing missing prereqs" section
#   -h, --help        print this help

set -euo pipefail

# --------------------------------------------------------------------
# Pretty output helpers.
# --------------------------------------------------------------------
say() { printf "\033[1;36m==>\033[0m %s\n" "$*"; }
ok()  { printf "    \033[1;32mok\033[0m %s\n" "$*"; }
warn(){ printf "    \033[1;33m!!\033[0m %s\n" "$*"; }
die() { printf "\033[1;31m!!\033[0m %s\n" "$*" >&2; exit 1; }

# --------------------------------------------------------------------
# Arg parsing.
# --------------------------------------------------------------------
FORCE=0
RUN_DOCTOR=1
RUN_INIT=1
CHECK_PREREQS=1
BIN_DIR=""
MODE="auto"
VERSION_TAG="latest"
REPO_OWNER="AiSU-AI"
REPO_NAME="aidev"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bin)          BIN_DIR="$2"; shift 2 ;;
    --from-release) MODE="release"; shift ;;
    --from-source)  MODE="source"; shift ;;
    --version)      VERSION_TAG="$2"; shift 2 ;;
    --force)        FORCE=1; shift ;;
    --no-doctor)    RUN_DOCTOR=0; shift ;;
    --no-init)      RUN_INIT=0; shift ;;
    --no-prereqs)   CHECK_PREREQS=0; shift ;;
    -h|--help)
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "install.sh: unknown flag $1" >&2
      exit 1
      ;;
  esac
done

# --------------------------------------------------------------------
# Platform detection.
# --------------------------------------------------------------------
detect_os() {
  local uname_s
  uname_s="$(uname -s)"
  case "$uname_s" in
    Darwin) echo darwin ;;
    Linux)  echo linux ;;
    *)
      die "unsupported OS: $uname_s (aidev prebuilt binaries ship for macOS and Linux only)"
      ;;
  esac
}

detect_arch() {
  local uname_m
  uname_m="$(uname -m)"
  case "$uname_m" in
    arm64|aarch64) echo arm64 ;;
    x86_64|amd64)  echo amd64 ;;
    *)
      die "unsupported arch: $uname_m"
      ;;
  esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
ok "detected platform: ${OS}-${ARCH}"

# --------------------------------------------------------------------
# Decide install mode.
# --------------------------------------------------------------------
if [[ "$MODE" == "auto" ]]; then
  if command -v go >/dev/null 2>&1; then
    MODE="source"
  else
    MODE="release"
  fi
fi

say "Install mode: $MODE"

# --------------------------------------------------------------------
# Prereq check — warn about missing runtime deps but don't block.
# aidev doctor does the authoritative check; this is a head start
# so users see "install Ollama first" BEFORE the binary lands.
# --------------------------------------------------------------------
if [[ "$CHECK_PREREQS" -eq 1 ]]; then
  say "Checking runtime prerequisites"
  MISSING=()

  if ! command -v claude >/dev/null 2>&1; then
    MISSING+=("claude — needed for the medium and large tiers (default).")
    MISSING+=("    install: https://claude.ai/download")
  else
    ok "claude: $(command -v claude)"
  fi

  if ! command -v ollama >/dev/null 2>&1; then
    MISSING+=("ollama — needed for the small (local) tier.")
    if [[ "$OS" == "darwin" ]] && command -v brew >/dev/null 2>&1; then
      MISSING+=("    install: brew install ollama")
    else
      MISSING+=("    install: https://ollama.com/download")
    fi
  else
    ok "ollama: $(command -v ollama)"
  fi

  if [[ "${#MISSING[@]}" -gt 0 ]]; then
    printf "\n"
    warn "some runtime prerequisites are not installed:"
    for line in "${MISSING[@]}"; do
      warn "  $line"
    done
    warn "the aidev binary will install fine, but agent runs will FAIL until these are available"
    warn "aidev doctor at the end of this script will re-verify."
    printf "\n"
  fi
fi

# --------------------------------------------------------------------
# Pick a bin directory on PATH.
# --------------------------------------------------------------------
pick_bin_dir() {
  if [[ -n "$BIN_DIR" ]]; then
    echo "$BIN_DIR"
    return
  fi
  for candidate in "$HOME/.local/bin" "$HOME/bin" "/usr/local/bin"; do
    if [[ -d "$candidate" && -w "$candidate" ]] && echo "$PATH" | tr ':' '\n' | grep -qx "$candidate"; then
      echo "$candidate"
      return
    fi
  done
  echo "$HOME/.local/bin"
}

INSTALL_DIR="$(pick_bin_dir)"
INSTALLED_BIN="$INSTALL_DIR/aidev"
mkdir -p "$INSTALL_DIR"

if [[ -f "$INSTALLED_BIN" && "$FORCE" -ne 1 ]]; then
  warn "$INSTALLED_BIN exists — pass --force to overwrite"
  die  "refusing to overwrite existing binary"
fi

# --------------------------------------------------------------------
# Acquire the binary: either build from source or download from the
# GitHub release.
# --------------------------------------------------------------------
if [[ "$MODE" == "source" ]]; then
  say "Building aidev from source"
  command -v go >/dev/null 2>&1 || die "go not found on PATH. Either install Go 1.24+ or rerun with --from-release."
  ok "go: $(go version | awk '{print $3}')"

  SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"
  [[ -f "$SCRIPT_DIR/go.mod" ]] || die "build mode requires running install.sh from the aidev repo root (no go.mod in $SCRIPT_DIR)"
  cd "$SCRIPT_DIR"

  BUILD_OUT="$SCRIPT_DIR/aidev"
  go build \
    -ldflags "-s -w -X github.com/aisu-ai/aidev/internal/version.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    -o "$BUILD_OUT" \
    ./cmd/aidev
  ok "built: $BUILD_OUT ($(du -h "$BUILD_OUT" | awk '{print $1}'))"

  cp "$BUILD_OUT" "$INSTALLED_BIN"
  chmod +x "$INSTALLED_BIN"
  ok "installed: $INSTALLED_BIN"

elif [[ "$MODE" == "release" ]]; then
  say "Downloading aidev binary from GitHub Release"
  command -v curl >/dev/null 2>&1 || die "curl not found on PATH (required for release mode)"
  command -v tar  >/dev/null 2>&1 || die "tar not found on PATH (required for release mode)"

  # Resolve the latest version tag if the user didn't pin one.
  if [[ "$VERSION_TAG" == "latest" ]]; then
    say "Resolving latest release from github.com/${REPO_OWNER}/${REPO_NAME}"
    LATEST_JSON="$(curl -sSL -H "Accept: application/vnd.github+json" "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest" || true)"
    VERSION_TAG="$(echo "$LATEST_JSON" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
    if [[ -z "$VERSION_TAG" ]]; then
      die "could not resolve latest release tag. The repo may have no releases yet. Clone the repo and run './install.sh --from-source', or pass --version <tag> if you know one."
    fi
  fi
  ok "version: $VERSION_TAG"

  ARCHIVE="aidev-${VERSION_TAG}-${OS}-${ARCH}.tar.gz"
  URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${VERSION_TAG}/${ARCHIVE}"
  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$TMP_DIR"' EXIT

  say "Downloading $URL"
  if ! curl -fL --progress-bar "$URL" -o "$TMP_DIR/$ARCHIVE"; then
    die "download failed. Check https://github.com/${REPO_OWNER}/${REPO_NAME}/releases for available assets, or rerun with --from-source."
  fi
  ok "downloaded: $(du -h "$TMP_DIR/$ARCHIVE" | awk '{print $1}')"

  say "Extracting"
  tar -C "$TMP_DIR" -xzf "$TMP_DIR/$ARCHIVE"
  EXTRACTED="$TMP_DIR/aidev-${VERSION_TAG}-${OS}-${ARCH}/aidev"
  [[ -x "$EXTRACTED" ]] || die "extracted archive does not contain an executable at $EXTRACTED"

  cp "$EXTRACTED" "$INSTALLED_BIN"
  chmod +x "$INSTALLED_BIN"
  ok "installed: $INSTALLED_BIN"

else
  die "unknown mode: $MODE"
fi

# --------------------------------------------------------------------
# Warn if the install dir isn't on PATH.
# --------------------------------------------------------------------
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
  warn "$INSTALL_DIR is not on your PATH"
  warn "add this to your shell rc (~/.zshrc or ~/.bashrc):"
  warn "    export PATH=\"$INSTALL_DIR:\$PATH\""
  warn "then restart your shell and re-run: aidev doctor"
fi

# --------------------------------------------------------------------
# Install default config + Claude Code slash commands.
# --------------------------------------------------------------------
FORCE_FLAG=""
[[ "$FORCE" -eq 1 ]] && FORCE_FLAG="--force"

# Decide whether to run the interactive `aidev init` walkthrough or
# fall back to the non-interactive install-config + doctor path.
#
# Criteria for init:
#   - --no-init was NOT passed
#   - stdin is a TTY (so a `curl | bash` pipeline falls back gracefully)
#
# When init runs, it REPLACES the `aidev install` + `aidev doctor`
# steps below — init lays down the config AND runs doctor itself.
RUN_INIT_EFFECTIVE=0
if [[ "$RUN_INIT" -eq 1 ]] && [[ -t 0 ]]; then
  RUN_INIT_EFFECTIVE=1
fi

if [[ "$RUN_INIT_EFFECTIVE" -eq 1 ]]; then
  say "Starting aidev init (interactive setup)"
  INIT_FLAGS=()
  [[ "$FORCE" -eq 1 ]] && INIT_FLAGS+=("--force")
  if "$INSTALLED_BIN" init "${INIT_FLAGS[@]}"; then
    ok "init complete"
  else
    warn "aidev init did not complete cleanly — see output above"
    warn "rerun: $INSTALLED_BIN init"
  fi
else
  say "Installing default config"
  "$INSTALLED_BIN" install $FORCE_FLAG
  ok "config installed"
fi

say "Installing Claude Code slash commands"
"$INSTALLED_BIN" plugin install $FORCE_FLAG
ok "plugin commands installed"

# --------------------------------------------------------------------
# Smoke test. Skipped when init ran, because init already ran doctor
# itself as its final verification gate.
# --------------------------------------------------------------------
if [[ "$RUN_DOCTOR" -eq 1 ]] && [[ "$RUN_INIT_EFFECTIVE" -eq 0 ]]; then
  say "Running aidev doctor"
  if "$INSTALLED_BIN" doctor; then
    ok "doctor passed"
  else
    warn "doctor reported one or more issues — fix them per the output above"
    warn "rerun: $INSTALLED_BIN doctor"
  fi
fi

# --------------------------------------------------------------------
# Next steps.
# --------------------------------------------------------------------
cat <<EOF

================================================================================
  aidev is installed.

  Binary:  $INSTALLED_BIN
  Version: $("$INSTALLED_BIN" --version 2>/dev/null | awk '{print $2}')
  Config:  \${XDG_CONFIG_HOME:-\$HOME/.config}/aidev/
  Plugin:  \${CLAUDE_CONFIG_DIR:-\$HOME/.claude}/commands/aidev-*.md

  Next steps:
    1. Verify: aidev doctor
    2. (Optional) claude /login if you haven't authenticated Claude Code
    3. Configure (if skipped): aidev init
    4. Run on a real issue:
         aidev -issue https://github.com/your-org/your-repo/issues/42 -repo ~/code/your-repo
    5. Or from inside Claude Code:
         /aidev-run <issue-url> <repo-path>

  Uninstall:
    rm $INSTALLED_BIN
    rm -rf \${XDG_CONFIG_HOME:-\$HOME/.config}/aidev
    aidev plugin uninstall
================================================================================
EOF
