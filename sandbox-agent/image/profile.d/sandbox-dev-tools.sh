export NIX_REMOTE="${NIX_REMOTE:-daemon}"
export NIX_CONFIG="${NIX_CONFIG:-experimental-features = nix-command flakes}"
export NPM_CONFIG_PREFIX="${NPM_CONFIG_PREFIX:-$HOME/.npm-global}"
# The whole order, not a prepend onto what /etc/profile just set. Two reasons it
# has to be stated outright: /etc/profile overwrites PATH unconditionally and
# differently for root, so there is nothing here worth building on; and brew
# belongs in the *middle* of the list, which no amount of prepending reaches.
#
# Brew sits after /usr/local/bin, deliberately. That is where the image installs
# the shims that are load-bearing rather than convenient -- `docker`, which ADR
# 0044 §8 requires because `docker build` pins buildx's `default` instance and
# would otherwise silently build on the sandbox's own dockerd, and the nix shims
# of ADR 0075 §4. `brew install docker` is an ordinary thing to type and it
# installs a real Docker CLI; ahead of /usr/local/bin it would replace that shim
# for good, and the failure is a build that succeeds in the wrong place. Brew
# still precedes /usr/bin, which is the whole point of having it.
#
# Keep this in step with image.json's PATH, which is what a non-login exec gets.
export PATH="$HOME/.npm-global/bin:$HOME/.cargo/bin:$HOME/.nix-profile/bin:$HOME/.local/bin:/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:/usr/sbin:/usr/bin:/sbin:/bin"

if [ -e "$HOME/.nix-profile/etc/profile.d/nix.sh" ]; then
  . "$HOME/.nix-profile/etc/profile.d/nix.sh"
fi

# The rest of `brew shellenv`. PATH is set above rather than here so the whole
# order lives in one place, and it matches image.json's PATH, which is what a
# non-login exec gets — /etc/profile overwrites PATH outright, which is the
# reason this file exists at all.
#
# HOMEBREW_NO_AUTO_UPDATE is not a preference: the prefix is an overlay whose
# lower layer is image content, so brew's habit of `git pull`ing its own
# repository before an install would copy the whole repository up into this
# sandbox's data volume. Formula data still refreshes — that comes from the
# JSON API, which this does not touch.
export HOMEBREW_PREFIX="${HOMEBREW_PREFIX:-/home/linuxbrew/.linuxbrew}"
export HOMEBREW_CELLAR="${HOMEBREW_CELLAR:-$HOMEBREW_PREFIX/Cellar}"
export HOMEBREW_REPOSITORY="${HOMEBREW_REPOSITORY:-$HOMEBREW_PREFIX/Homebrew}"
export HOMEBREW_NO_AUTO_UPDATE="${HOMEBREW_NO_AUTO_UPDATE:-1}"
export HOMEBREW_NO_ANALYTICS="${HOMEBREW_NO_ANALYTICS:-1}"
export MANPATH="$HOMEBREW_PREFIX/share/man${MANPATH+:$MANPATH}:"
export INFOPATH="$HOMEBREW_PREFIX/share/info${INFOPATH:+:$INFOPATH}"
