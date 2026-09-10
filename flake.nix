{
  description = "Discobox development environment and libkrun pool VM runtime";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      # The libkrun runtime is Linux-only, so packages and checks stay on one
      # system. Development shells fan out further because check, test, and
      # release now run out of this flake on every platform the project targets
      # (ADR 0066 §3).
      buildSystems = [ "x86_64-linux" ];
      devSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forBuildSystems = nixpkgs.lib.genAttrs buildSystems;
      forDevSystems = nixpkgs.lib.genAttrs devSystems;
      overriddenLibkrun =
        pkgs:
        pkgs.libkrun.override {
          withBlk = true;
          withNet = true;
        };
    in
    {
      # The only thing this flake builds is what a machine needs installed to
      # run a libkrun pool: the library the launcher dlopens, and passt. There
      # is no launcher package any more — the launcher is a hidden subcommand of
      # the server binary (ADR 0062 §9) — and no image builders, because the
      # guest image and its kernel are built by Docker from vm-image/ and pulled
      # from a registry at run time (ADR 0062 §3, §5, §6).
      packages = forBuildSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          libkrun-runtime = pkgs.buildEnv {
            name = "discobox-libkrun-runtime";
            paths = [
              (overriddenLibkrun pkgs)
              pkgs.passt
            ];
          };
        in
        {
          inherit libkrun-runtime;
          default = libkrun-runtime;
        }
      );

      checks = forBuildSystems (system: {
        inherit (self.packages.${system}) libkrun-runtime;
      });

      devShells = forDevSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          inherit (pkgs.stdenv.hostPlatform) isLinux;
          # mkShell injects Nix's clang, and the darwin build has to compile
          # Objective-C against Virtualization.framework and sign with
          # /usr/bin/codesign. On the one platform where Apple owns the SDK,
          # defer to the system Xcode toolchain (ADR 0066 §3).
          mkDevShell = if isLinux then pkgs.mkShell else pkgs.mkShellNoCC;
          # Everything `go tool task <target>` needs. `task`, `golangci-lint`,
          # and `ogen` are deliberately absent: go.mod already pins them as tool
          # dependencies, and two pins drift (ADR 0066 §3).
          #
          # It is a value rather than an inline shell because the libkrun shell
          # extends it. A shell that added libkrun and dropped the toolchain
          # could not run `task dev`, which is the one thing anyone enters it to
          # do.
          baseShell = {
            packages = [
              pkgs.go
              pkgs.git
              pkgs.gh
              pkgs.jq
              # `release:windows-zip` builds the archive winget installs from,
              # and `scripts/winget-manifests.sh` reads back the one a release
              # actually uploaded to check it holds what the manifest claims.
              pkgs.zip
              pkgs.unzip
              pkgs.bats
              pkgs.shellcheck
              pkgs.nodejs
              pkgs.pnpm
              pkgs.docker-client
              pkgs.docker-buildx
            ]
            ++ pkgs.lib.optionals isLinux [
              # `guest:verify` reads the assembled root filesystem with
              # dumpe2fs; the guest image itself is built by Docker.
              pkgs.e2fsprogs
            ];

            # Nix supplies a bootstrap Go only. The toolchain that actually
            # compiles is the one go.mod names, fetched on demand.
            GOTOOLCHAIN = "auto";

            # The Docker CLI searches fixed plugin directories, never PATH, so a
            # Nix-provided `docker buildx` is invisible without this.
            DOCKER_CLI_PLUGIN_EXTRA_DIRS = "${pkgs.docker-buildx}/libexec/docker/cli-plugins";

            shellHook = ''
              DISCOBOX_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
              export DISCOBOX_ROOT
              export DISCOBOX_COMPLETION_DIR="$DISCOBOX_ROOT/build/completions"
              export DISCOBOX_BASH_COMPLETION="$DISCOBOX_COMPLETION_DIR/discobox.bash"
              export DISCOBOX_ZSH_COMPLETION="$DISCOBOX_COMPLETION_DIR/_discobox"
              export DISCOBOX_FISH_COMPLETION="$DISCOBOX_COMPLETION_DIR/discobox.fish"
              export DISCOBOX_BASH_COMPLETION_USER_DIR="''${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions"
              # `task build` writes here, and the built CLI is what a developer
              # runs against `task dev`.
              export PATH="$DISCOBOX_ROOT/build:$PATH"

              # Completions come out of the CLI itself, so they are refreshed
              # whenever one has been built and skipped silently before the
              # first build.
              if [ -x "$DISCOBOX_ROOT/build/discobox" ]; then
                mkdir -p "$DISCOBOX_COMPLETION_DIR" "$DISCOBOX_BASH_COMPLETION_USER_DIR"
                "$DISCOBOX_ROOT/build/discobox" completion bash > "$DISCOBOX_BASH_COMPLETION"
                "$DISCOBOX_ROOT/build/discobox" completion zsh > "$DISCOBOX_ZSH_COMPLETION"
                "$DISCOBOX_ROOT/build/discobox" completion fish > "$DISCOBOX_FISH_COMPLETION"
                cp "$DISCOBOX_BASH_COMPLETION" "$DISCOBOX_BASH_COMPLETION_USER_DIR/discobox"
              fi
            '';
          };
        in
        {
          default = mkDevShell baseShell;
        }
        # Running a libkrun pool needs libkrun itself, built with block and
        # network support, and passt. That override is not the derivation
        # cache.nixos.org has, so it stays out of the default shell rather than
        # making every CI job build libkrun from source (ADR 0066 §3): a
        # developer who runs libkrun pools enters this shell, and everyone else
        # never builds it.
        #
        # LD_LIBRARY_PATH is how the launcher finds the library. It dlopens
        # libkrun.so.1 by soname rather than linking it, so nothing resolves it
        # at build time and the loader's search path is the whole mechanism. A
        # machine outside this shell names the path in the provider's
        # libkrunPath instead.
        // nixpkgs.lib.optionalAttrs (builtins.elem system buildSystems) {
          libkrun = mkDevShell (
            baseShell
            // {
              packages = baseShell.packages ++ [
                (overriddenLibkrun pkgs)
                pkgs.passt
              ];
              LD_LIBRARY_PATH = "${overriddenLibkrun pkgs}/lib";
            }
          );
        }
      );

      formatter = forDevSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
