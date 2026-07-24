{
  description = "Discobox local libkrun VM runtime";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs, ... }:
    let
      systems = [ "x86_64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          libkrun = pkgs.libkrun.override {
            withBlk = true;
            withNet = true;
          };
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

      apps = forAllSystems (
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

      checks = forAllSystems (system: {
        inherit (self.packages.${system}) discobox-krun kernel-builder root-image-builder;
      });

      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          libkrun = pkgs.libkrun.override {
            withBlk = true;
            withNet = true;
          };
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.cargo
              pkgs.clippy
              pkgs.rustc
              pkgs.rustfmt
              pkgs.pkg-config
              libkrun
              pkgs.passt
              pkgs.docker-client
              pkgs.docker-buildx
              pkgs.e2fsprogs
              pkgs.qemu-utils
            ];
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
