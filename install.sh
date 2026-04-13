#!/usr/bin/env bash
# install.sh — one-command installer for aidev.
#
# Responsibilities:
#   1. Sanity-check the environment (Go, git repo root)
#   2. `go build` the binary
#   3. Detect a writable bin dir on PATH and copy the binary there
#   4. Run `aidev install` to lay down ~/.config/aidev/{models,principles}.yaml
#   5. Run `aidev plugin install` to copy slash commands into ~/.claude/commands/
#   6. Run `aidev doctor` as a smoke test
#   7. Print next-step guidance
#
# Usage:
#   ./install.sh              # interactive-ish, picks sensible defaults
#   ./install.sh --bin ~/bin  # override bin directory
#   ./install.sh --force      # overwrite existing binary / config / plugin files
#   ./install.sh --no-doctor  # skip the final smoke test
#
# Exit codes:
#   0 on success
#   1 on any failure (build, copy, config install, plugin install, doctor)

set -euo pipefail

FORCE=0
RUN_DOCTOR=1
BIN_DIR=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --bin)       BIN_DIR="$2"; shift 2 ;;
    --force)     FORCE=1; shift ;;
    --no-doctor) RUN_DOCTOR=0; shift ;;
    -h|--help)
      sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "install.sh: unknown flag $1" >&2
      exit 1
      ;;
  esac
done

say() { printf "\033[1;36m==>\033[0m %s\n" "$*"; }
ok()  { printf "    \033[1;32mok\033[0m %s\n" "$*"; }
warn(){ printf "    \033[1;33m!!\033[0m %s\n" "$*"; }
die() { printf "\033[1;31m!!\033[0m %s\n" "$*" >&2; exit 1; }

# 1. Environment sanity.
say "Checking environment"
command -v go >/dev/null 2>&1 || die "go not found on PATH. Install Go 1.24+ from https://go.dev/dl/"
ok "go: $(go version | awk '{print $3}')"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"
cd "$SCRIPT_DIR"
[[ -f go.mod ]] || die "install.sh must run from the aidev repo root (no go.mod in $SCRIPT_DIR)"
ok "repo root: $SCRIPT_DIR"

# 2. Build the binary.
say "Building aidev"
BUILD_OUT="$SCRIPT_DIR/aidev"
go build -o "$BUILD_OUT" ./cmd/aidev
[[ -x "$BUILD_OUT" ]] || die "build succeeded but $BUILD_OUT is missing or not executable"
ok "built: $BUILD_OUT ($(du -h "$BUILD_OUT" | awk '{print $1}'))"

# 3. Pick a bin directory on PATH.
#
# Preference order when --bin isn't passed:
#   ~/.local/bin  (XDG convention, common on modern macOS/Linux)
#   ~/bin         (traditional personal bin)
#   /usr/local/bin (if writable — rare but possible)
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
  # None of the usual suspects are ready. Create ~/.local/bin and add it
  # to PATH in the user's shell rc.
  echo "$HOME/.local/bin"
}

INSTALL_DIR="$(pick_bin_dir)"
say "Installing binary to $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
INSTALLED_BIN="$INSTALL_DIR/aidev"
if [[ -f "$INSTALLED_BIN" && "$FORCE" -ne 1 ]]; then
  warn "$INSTALLED_BIN exists — pass --force to overwrite, or remove it manually"
  die  "refusing to overwrite existing binary"
fi
cp "$BUILD_OUT" "$INSTALLED_BIN"
chmod +x "$INSTALLED_BIN"
ok "installed: $INSTALLED_BIN"

# Check if the install dir is actually on PATH, warn if not.
if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
  warn "$INSTALL_DIR is not on your PATH"
  warn "add this to your shell rc (~/.zshrc or ~/.bashrc):"
  warn "    export PATH=\"$INSTALL_DIR:\$PATH\""
  warn "then restart your shell and re-run: aidev doctor"
fi

# 4. Install shipped default config to ~/.config/aidev.
say "Installing default config"
FORCE_FLAG=""
[[ "$FORCE" -eq 1 ]] && FORCE_FLAG="--force"
"$INSTALLED_BIN" install $FORCE_FLAG
ok "config installed"

# 5. Install Claude Code slash commands.
say "Installing Claude Code slash commands"
if "$INSTALLED_BIN" plugin install $FORCE_FLAG 2>&1 | grep -q "claude"; then
  ok "plugin commands installed"
else
  "$INSTALLED_BIN" plugin install $FORCE_FLAG
  ok "plugin commands installed"
fi

# 6. Smoke test: run `aidev doctor` to confirm the install is usable.
if [[ "$RUN_DOCTOR" -eq 1 ]]; then
  say "Running aidev doctor"
  if "$INSTALLED_BIN" doctor; then
    ok "doctor passed"
  else
    warn "doctor reported one or more issues — fix them per the output above"
    warn "rerun: $INSTALLED_BIN doctor"
  fi
fi

# 7. Next steps.
cat <<EOF

================================================================================
  aidev is installed.

  Binary:  $INSTALLED_BIN
  Config:  \${XDG_CONFIG_HOME:-\$HOME/.config}/aidev/
  Plugin:  \${CLAUDE_CONFIG_DIR:-\$HOME/.claude}/commands/aidev-*.md

  Next steps:
    1. Verify: aidev doctor
    2. (Optional) \`claude /login\` if you haven't authenticated Claude Code
    3. Run on a real issue:
         aidev -issue https://github.com/your-org/your-repo/issues/42 -repo ~/code/your-repo
    4. Or from inside Claude Code:
         /aidev-run <issue-url> <repo-path>

  Uninstall:
    rm $INSTALLED_BIN
    rm -rf \${XDG_CONFIG_HOME:-\$HOME/.config}/aidev
    aidev plugin uninstall   # or rm \$HOME/.claude/commands/aidev-*.md
================================================================================
EOF
