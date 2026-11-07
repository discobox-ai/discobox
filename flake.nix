{
  description = "Discobox development environment and libkrun pool VM runtime";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      # The libkrun runtime is Linux-only, so packages and checks stay on one
      # system. Development shells fan out further, to every platform a
      # developer works on (ADR 0066 §3). CI enters one only on Linux; its macOS
      # job uses plain Go (ADR 26-11-02-173).
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
              pkgs.python3 # Docker UX smoke orchestration (stdlib only).
              # `release:windows-zip` builds the archive winget installs from,
              # and `scripts/winget-manifests.sh` reads back the one a release
              # actually uploaded to check it holds what the manifest claims.
              pkgs.zip
              pkgs.unzip
              pkgs.bats
              pkgs.shellcheck
              # pkg/installer/'s tests run install.ps1 wherever PowerShell is
              # found. The Windows runner has its own; this puts it on every
              # other one, so the script is not tested on Windows alone.
              pkgs.powershell
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
              # Inside a discobox, DISCOBOX_SERVER names the server that runs
              # the box (api.discobox.internal). In this checkout, the CLI
              # should reach the local `task dev` server instead. Only there: a
              # developer's own DISCOBOX_SERVER on their machine is theirs.
              if [ -e /etc/discobox/sandbox.json ]; then
                unset DISCOBOX_SERVER
              fi
              # The local server's socket defaults under TMPDIR, which this
              # shell replaces with a nix-shell.XXXXXX directory of its own —
              # so a server started here would be invisible to every other
              # shell, and this shell's CLI would miss the one already running.
              # Pin the address a plain shell resolves (XDG_RUNTIME_DIR, else
              # the user's real temp directory) on both ends; an address the
              # developer set themselves is left alone.
              if [ -z "''${DISCOBOX_SERVER_ADDRESS:-}" ]; then
                if [ -n "''${XDG_RUNTIME_DIR:-}" ]; then
                  _discobox_runtime="$XDG_RUNTIME_DIR"
                elif [ "$(uname -s)" = Darwin ]; then
                  _discobox_runtime="$(getconf DARWIN_USER_TEMP_DIR)"
                  _discobox_runtime="''${_discobox_runtime%/}/discobox-$(id -u)"
                else
                  _discobox_runtime="/tmp/discobox-$(id -u)"
                fi
                export DISCOBOX_SERVER_ADDRESS="$(python3 -c 'import sys, urllib.parse; print("unix://" + urllib.parse.quote(sys.argv[1], safe="/"))' "$_discobox_runtime/discobox/server.sock")"
                unset _discobox_runtime
              fi

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
        # The desktop app (desktop/, ADR 26-11-07-876) is Rust on Rune. Like the
        # libkrun shell it extends the base rather than growing it, so nobody
        # who never builds the app pays for a Rust toolchain and a window
        # system's libraries.
        #
        # Rust is handled as Go is: Nix supplies rustup only, and the
        # toolchain that compiles is the one desktop/rust-toolchain.toml
        # names, fetched on first use.
        #
        # On Linux the app links xkbcommon and Wayland (found through
        # pkg-config) and loads the X11 libraries and the Vulkan loader at run
        # time, so they are on LD_LIBRARY_PATH. Mesa's software Vulkan driver
        # comes too, for headless rendering where there is no GPU: name it with
        # VK_ICD_FILENAMES="$DISCOBOX_LAVAPIPE_ICD". It is not the default,
        # because a machine with a GPU should use it.
        // {
          desktop =
            let
              runtimeLibs = pkgs.lib.optionals isLinux [
                pkgs.libxkbcommon
                pkgs.wayland
                pkgs.vulkan-loader
                pkgs.libx11
                pkgs.libxcursor
                pkgs.libxi
                pkgs.libxrandr
              ];
            in
            mkDevShell (
              baseShell
              // {
                packages =
                  baseShell.packages
                  ++ [ pkgs.rustup ]
                  ++ pkgs.lib.optionals isLinux (
                    [
                      pkgs.pkg-config
                      pkgs.mesa
                    ]
                    ++ runtimeLibs
                  );
                shellHook =
                  baseShell.shellHook
                  + pkgs.lib.optionalString isLinux ''
                    export LD_LIBRARY_PATH="${pkgs.lib.makeLibraryPath runtimeLibs}''${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
                    export DISCOBOX_LAVAPIPE_ICD="${pkgs.mesa}/share/vulkan/icd.d/lvp_icd.${pkgs.stdenv.hostPlatform.uname.processor}.json"
                  '';
              }
            );
        }
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
