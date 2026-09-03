{
  description = "Discobox development environment and local libkrun VM runtime";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      # The libkrun artifacts are Linux-only, so packages/apps/checks stay on
      # one system. Development shells fan out further because check, test, and
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
      packages = forBuildSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          libkrun = overriddenLibkrun pkgs;
          discobox-krun = pkgs.rustPlatform.buildRustPackage {
            pname = "discobox-krun";
            version = "0.1.0";
            src = ./server/providers/libkrun/launcher;

            cargoLock.lockFile = ./server/providers/libkrun/launcher/Cargo.lock;

            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = [ libkrun ];

            PASST_PATH = "${pkgs.passt}/bin/passt";

            meta = {
              description = "Discobox libkrun microVM launcher";
              license = pkgs.lib.licenses.asl20;
              mainProgram = "discobox-krun";
              platforms = [ "x86_64-linux" ];
            };
          };
          root-image-builder = pkgs.writeShellApplication {
            name = "discobox-build-root-image";
            runtimeInputs = [
              pkgs.bash
              pkgs.coreutils
              pkgs.docker-client
              pkgs.docker-buildx
              pkgs.e2fsprogs
              pkgs.fakeroot
              pkgs.gitMinimal
              pkgs.gnutar
              pkgs.qemu-utils
            ];
            text = builtins.readFile ./server/providers/libkrun/image/build-root-image.sh;
          };
          kernel-builder = pkgs.writeShellApplication {
            name = "discobox-build-kernel";
            runtimeInputs = [
              pkgs.bash
              pkgs.coreutils
              pkgs.docker-client
              pkgs.docker-buildx
              pkgs.gitMinimal
            ];
            text = builtins.readFile ./server/providers/libkrun/kernel/build-kernel.sh;
          };
          libkrun-runtime = pkgs.buildEnv {
            name = "discobox-libkrun-runtime";
            paths = [
              discobox-krun
              pkgs.e2fsprogs
            ];
          };
        in
        {
          inherit
            discobox-krun
            kernel-builder
            libkrun-runtime
            root-image-builder
            ;
          image-builder = root-image-builder;
          default = libkrun-runtime;
        }
      );

      apps = forBuildSystems (
        system:
        let
          program = "${self.packages.${system}.discobox-krun}/bin/discobox-krun";
        in
        {
          discobox-krun = {
            type = "app";
            inherit program;
            meta.description = "Run or validate a Discobox libkrun microVM";
          };
          build-root-image = {
            type = "app";
            program = "${self.packages.${system}.root-image-builder}/bin/discobox-build-root-image";
            meta.description = "Build the Discobox libkrun root QCOW2 image";
          };
          build-kernel = {
            type = "app";
            program = "${self.packages.${system}.kernel-builder}/bin/discobox-build-kernel";
            meta.description = "Build the Discobox libkrun Linux kernel with Docker";
          };
          default = {
            type = "app";
            inherit program;
            meta.description = "Run or validate a Discobox libkrun microVM";
          };
        }
      );

      checks = forBuildSystems (system: {
        inherit (self.packages.${system}) discobox-krun kernel-builder root-image-builder;
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
        in
        {
          # Everything `go tool task <target>` needs. `task`, `golangci-lint`,
          # and `ogen` are deliberately absent: go.mod already pins them as tool
          # dependencies, and two pins drift (ADR 0066 §3).
          default = mkDevShell {
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
        }
        # Working on the libkrun launcher, and nothing else, needs Rust and a
        # libkrun built with block and network support. That override is not the
        # derivation cache.nixos.org has, so it stays out of the default shell
        # rather than making every CI job build libkrun from source (ADR 0066
        # §3). `nix build .#discobox-krun` needs neither shell.
        // nixpkgs.lib.optionalAttrs (builtins.elem system buildSystems) {
          libkrun = pkgs.mkShell {
            packages = [
              pkgs.cargo
              pkgs.clippy
              pkgs.rustc
              pkgs.rustfmt
              pkgs.pkg-config
              (overriddenLibkrun pkgs)
              pkgs.passt
              pkgs.docker-client
              pkgs.docker-buildx
              pkgs.e2fsprogs
              pkgs.qemu-utils
            ];
          };
        }
      );

      formatter = forDevSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
