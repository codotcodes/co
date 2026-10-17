{
  description = "co: the co.codes command-line client";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        package = pkgs.rustPlatform.buildRustPackage {
          pname = "co-codes-cli";
          version = (builtins.fromTOML (builtins.readFile ./Cargo.toml)).package.version;
          src = self;
          cargoLock.lockFile = ./Cargo.lock;
          nativeCheckInputs = [ pkgs.git ];
          meta = {
            description = "Human-facing CLI for co.codes";
            homepage = "https://co.codes";
            license = with pkgs.lib.licenses; [ mit asl20 ];
            mainProgram = "co";
            platforms = pkgs.lib.platforms.unix;
          };
        };
      in {
        packages = {
          co = package;
          default = package;
        };

        apps = {
          co = { type = "app"; program = pkgs.lib.getExe package; };
          default = { type = "app"; program = pkgs.lib.getExe package; };
        };

        devShells.default = pkgs.mkShell {
          packages = [ pkgs.cargo pkgs.rustc pkgs.rustfmt pkgs.clippy pkgs.shellcheck ];
        };
      });
}
