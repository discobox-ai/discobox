#!/bin/bash
#---
# name: Setup Nix and direnv
# type: session
# description: Install Nix, ensure it is on PATH, enable direnv in bash, and allow the workspace .envrc.
#---

set -euo pipefail

USER_HOME="${HOME:?HOME must be set}"
WORKSPACE="${DISCOBOX_WORKSPACE:-$(pwd)}"
PROFILE="$USER_HOME/.nix-profile"
BASHRC="$USER_HOME/.bashrc"

log() {
  echo "[setup-nix-direnv] $*"
}

ensure_bashrc_exists() {
  if [ ! -e "$BASHRC" ]; then
    install -m 0644 /dev/null "$BASHRC"
  fi
}

ensure_bashrc_block() {
  ensure_bashrc_exists

  local begin="# >>> discobox nix-direnv setup >>>"
  local end="# <<< discobox nix-direnv setup <<<"
  local block
  block=$(cat <<'EOF'
# >>> discobox nix-direnv setup >>>
# Keep Nix and direnv available in Discobox shells.
if [ -e "$HOME/.nix-profile/etc/profile.d/nix.sh" ]; then
  . "$HOME/.nix-profile/etc/profile.d/nix.sh"
elif [ -d "$HOME/.nix-profile/bin" ]; then
  export PATH="$HOME/.nix-profile/bin:$PATH"
fi

if command -v direnv >/dev/null 2>&1; then
  eval "$(direnv hook bash)"
fi
# <<< discobox nix-direnv setup <<<
EOF
)

  local tmp blockfile
  tmp=$(mktemp)
  blockfile=$(mktemp)
  printf '%s\n' "$block" > "$blockfile"
  if grep -Fq "$begin" "$BASHRC" && grep -Fq "$end" "$BASHRC"; then
    # The block reaches awk as a file rather than through -v. A -v value
    # carrying newlines is a GNU extension, and BSD awk -- which is the awk on
    # macOS -- rejects it outright with "newline in string", so the replace
    # path failed on every run after the one that first wrote the block.
    awk -v begin="$begin" -v end="$end" -v blockfile="$blockfile" '
      $0 == begin {
        while ((getline line < blockfile) > 0) print line
        close(blockfile)
        skip = 1
        next
      }
      $0 == end { skip = 0; next }
      !skip { print }
    ' "$BASHRC" > "$tmp"
  else
    {
      printf '%s\n\n' "$block"
      cat "$BASHRC"
    } > "$tmp"
  fi
  cat "$tmp" > "$BASHRC"
  rm -f "$tmp" "$blockfile"
}

load_nix_profile() {
  if [ -e "$PROFILE/etc/profile.d/nix.sh" ]; then
    # shellcheck disable=SC1091
    . "$PROFILE/etc/profile.d/nix.sh"
  elif [ -d "$PROFILE/bin" ]; then
    export PATH="$PROFILE/bin:$PATH"
  fi
}

install_existing_store_nix() {
  local nix_bin nix_pkg
  nix_bin=$(find /nix/store -maxdepth 3 -path '*/bin/nix' -type f 2>/dev/null | sort -V | tail -n 1 || true)
  if [ -z "$nix_bin" ]; then
    return 1
  fi

  nix_pkg=$(dirname "$(dirname "$nix_bin")")
  log "linking existing Nix package from $nix_pkg"
  export PATH="$(dirname "$nix_bin"):$PATH"
  export NIX_REMOTE=local
  "$nix_pkg/bin/nix-env" -p "$PROFILE" -i "$nix_pkg" >/dev/null
}

install_nix() {
  load_nix_profile
  if command -v nix >/dev/null 2>&1; then
    log "Nix already installed: $(nix --version)"
    return
  fi

  if [ -d /nix/store ] && install_existing_store_nix; then
    load_nix_profile
    log "Nix installed from existing store: $(nix --version)"
    return
  fi

  # macOS has no unattended install left for this hook to do. Upstream removed
  # single-user mode there -- "--no-daemon installs are no-longer supported on
  # Darwin/macOS" -- and the daemon install that replaced it is a system change
  # (an APFS volume, a _nixbld group, a launchd daemon) that needs root. A
  # session hook running in the background is the wrong place to ask for that,
  # so name the command and leave the machine alone.
  if [ "$(uname -s)" = "Darwin" ]; then
    log "no Nix here, and macOS has no unattended install this hook can perform"
    log "to install it: curl -fsSL https://nixos.org/nix/install | sh -s -- --daemon"
    return 1
  fi

  log "installing Nix with the upstream no-daemon installer"
  curl -fsSL https://nixos.org/nix/install | sh -s -- --no-daemon --yes --no-modify-profile
  load_nix_profile
  log "Nix installed: $(nix --version)"
}

install_direnv() {
  load_nix_profile
  if command -v direnv >/dev/null 2>&1; then
    log "direnv already installed: $(direnv version)"
    return
  fi

  log "installing direnv with Nix"
  nix --extra-experimental-features 'nix-command flakes' \
    profile install --profile "$PROFILE" nixpkgs#direnv
  load_nix_profile
  log "direnv installed: $(direnv version)"
}

allow_workspace_envrc() {
  load_nix_profile
  if command -v direnv >/dev/null 2>&1 && [ -f "$WORKSPACE/.envrc" ]; then
    log "allowing $WORKSPACE/.envrc"
    (cd "$WORKSPACE" && direnv allow .)
  fi
}

ensure_bashrc_block

# Everything below needs Nix: direnv is installed with it, and .envrc loads the
# flake through it. Without Nix there is nothing here to do, and nothing wrong
# either -- the toolchain can come from elsewhere -- so this is a skip, not a
# failure.
if ! install_nix; then
  log "skipping direnv and .envrc: both need Nix"
  exit 0
fi

install_direnv
ensure_bashrc_block
allow_workspace_envrc

log "Nix and direnv setup complete"
