export NIX_REMOTE="${NIX_REMOTE:-daemon}"
export NIX_CONFIG="${NIX_CONFIG:-experimental-features = nix-command flakes}"
export NPM_CONFIG_PREFIX="${NPM_CONFIG_PREFIX:-$HOME/.npm-global}"
export PATH="$HOME/.npm-global/bin:$HOME/.cargo/bin:$HOME/.nix-profile/bin:$HOME/.local/bin:/nix/var/nix/profiles/default/bin:/usr/local/go/bin:$PATH"

if [ -e "$HOME/.nix-profile/etc/profile.d/nix.sh" ]; then
  . "$HOME/.nix-profile/etc/profile.d/nix.sh"
fi
