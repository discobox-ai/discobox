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
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              delve
              git
              gnumake
              go_1_26
              go-task
              gopls
              gotools
              postgresql
              sqlite
            ];

            env = {
              GOTOOLCHAIN = "local";
            };

            shellHook = ''
              export DISCOBOX_ROOT="$PWD"
            '';
          };
        }
      );
    };
}
