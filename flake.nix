{
  description = "mautrix-chatto: a Matrix-Chatto puppeting bridge";

  # The channel tarball works without GitHub API access and pins to an
  # immutable releases.nixos.org URL in flake.lock.
  inputs.nixpkgs.url = "https://channels.nixos.org/nixos-unstable/nixexprs.tar.xz";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      rev = self.shortRev or self.dirtyShortRev or "unknown";
    in
    {
      overlays.default = final: _prev: {
        mautrix-chatto = final.callPackage ./nix/package.nix { inherit rev; };
      };

      packages = forAllSystems (pkgs: rec {
        mautrix-chatto = pkgs.callPackage ./nix/package.nix { inherit rev; };
        default = mautrix-chatto;
      });

      nixosModules = rec {
        mautrix-chatto = {
          imports = [ ./nix/module.nix ];
          nixpkgs.overlays = [ self.overlays.default ];
        };
        default = mautrix-chatto;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            golangci-lint
            buf
            protoc-gen-go
            protoc-gen-connect-go
            nodejs
          ];
        };
      });

      checks = forAllSystems (
        pkgs:
        let
          pkg = self.packages.${pkgs.stdenv.hostPlatform.system}.mautrix-chatto;
        in
        {
          build = pkg;
          tests = pkg.overrideAttrs (old: {
            pname = "mautrix-chatto-tests";
            subPackages = [ ];
            doCheck = true;
            # Only run the tests; nothing is installed.
            buildPhase = "runHook preBuild; runHook postBuild";
            checkPhase = ''
              runHook preCheck
              export HOME=$TMPDIR
              go test -tags goolm -count=1 ./...
              runHook postCheck
            '';
            installPhase = "touch $out";
          });
          lint = pkg.overrideAttrs (old: {
            pname = "mautrix-chatto-lint";
            src = pkgs.lib.fileset.toSource {
              root = ./.;
              fileset = pkgs.lib.fileset.unions [
                ./go.mod
                ./go.sum
                ./cmd
                ./pkg
                ./.golangci.yml
              ];
            };
            nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.golangci-lint ];
            buildPhase = ''
              runHook preBuild
              export HOME=$TMPDIR GOLANGCI_LINT_CACHE=$TMPDIR/lint-cache
              golangci-lint run ./...
              runHook postBuild
            '';
            installPhase = "touch $out";
          });
          nixfmt = pkgs.runCommand "nixfmt-check" { nativeBuildInputs = [ pkgs.nixfmt ]; } ''
            nixfmt --check ${./flake.nix} ${./nix/package.nix} ${./nix/module.nix}
            touch $out
          '';
        }
        // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          # Evaluates the NixOS module and builds the resulting unit file.
          module =
            (nixpkgs.lib.nixosSystem {
              inherit (pkgs.stdenv.hostPlatform) system;
              modules = [
                self.nixosModules.default
                {
                  boot.isContainer = true;
                  system.stateVersion = "26.05";
                  services.mautrix-chatto = {
                    enable = true;
                    settings.network.server_url = "https://chat.example.com";
                    settings.homeserver.domain = "example.com";
                    metrics.enable = true;
                  };
                  services.prometheus.enable = true;
                }
              ];
            }).config.systemd.units."mautrix-chatto.service".unit;
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
