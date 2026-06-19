{
  description = "Development shell for discobox";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-darwin"
        "x86_64-linux"
      ];

      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          nodejs = pkgs.nodejs_24;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              corepack
              delve
              git
              gnumake
              go_1_26
              go-task
              gopls
              gotools
              nodejs
              postgresql
              sqlite
            ];

            env = {
              GOTOOLCHAIN = "local";
            };

            shellHook = ''
              export DISCOBOX_ROOT="$PWD"
              unset GOROOT
              export COREPACK_HOME="''${XDG_CACHE_HOME:-$HOME/.cache}/discobox-corepack"
              export PATH="$COREPACK_HOME/bin:$PATH"
              mkdir -p "$COREPACK_HOME/bin"
              corepack enable --install-directory "$COREPACK_HOME/bin"
              corepack prepare pnpm@11.4.0 --activate
            '';
          };
        }
      );
    };
}
