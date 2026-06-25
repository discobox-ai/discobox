export PATH="/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:$PATH"

if [ -e /root/.nix-profile/etc/profile.d/nix.sh ]; then
  . /root/.nix-profile/etc/profile.d/nix.sh
fi
