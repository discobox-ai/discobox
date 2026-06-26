export NIX_REMOTE="${NIX_REMOTE:-daemon}"
export NIX_CONFIG="${NIX_CONFIG:-experimental-features = nix-command flakes}"
export NPM_CONFIG_PREFIX="${NPM_CONFIG_PREFIX:-/root/.npm-global}"
export PNPM_HOME="${PNPM_HOME:-/var/lib/discobox/pnpm}"
export PATH="/root/.cargo/bin:/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/root/.local/bin:/root/.npm-global/bin:$PATH"

if [ -e /root/.nix-profile/etc/profile.d/nix.sh ]; then
  . /root/.nix-profile/etc/profile.d/nix.sh
fi
