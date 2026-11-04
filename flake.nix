{
  description = "co: the co.codes command-line client";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        package = pkgs.buildGo126Module {
          pname = "co-codes-cli";
          version = builtins.head (
            builtins.match ".*var Version = \"([^\"]+)\".*" (builtins.readFile ./internal/cli/version.go)
          );
          src = self;
          vendorHash = "sha256-5NFjKo59hfV0Y+2pQ+1URJHrIZu6DsuABrA93Q3v7gY=";
          subPackages = [ "cmd/co" ];
          env.CGO_ENABLED = 0;
          ldflags = [
            "-s"
            "-w"
          ];
          nativeCheckInputs = [
            pkgs.git
            pkgs.python3
          ];
          checkPhase = ''
            runHook preCheck
            export CO_TEST_GO_BINARY="$GOPATH/bin/co"
            go test ./... -count=1
            runHook postCheck
          '';
          meta = {
            description = "Human-facing CLI for co.codes";
            homepage = "https://co.codes";
            license = with pkgs.lib.licenses; [
              mit
              asl20
            ];
            mainProgram = "co";
            platforms = pkgs.lib.platforms.unix;
          };
        };
      in
      {
        packages = {
          co = package;
          default = package;
        };

        apps = {
          co = {
            type = "app";
            program = pkgs.lib.getExe package;
          };
          default = {
            type = "app";
            program = pkgs.lib.getExe package;
          };
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go_1_26
            pkgs.git
            pkgs.nodejs_24
            pkgs.shellcheck
          ];
        };
      }
    );
}
