{
  lib,
  buildGoModule,
  version ? "0.1.0",
  rev ? "unknown",
}:

buildGoModule {
  pname = "mautrix-chatto";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../pkg
    ];
  };

  # Update with `nix build .#mautrix-chatto 2>&1 | grep got:` after changing go.mod.
  vendorHash = "sha256-szbG0WrBimi9GZca7zhPx6wmQIhymit3esiXt+SbY/k=";

  # goolm is the pure-Go olm implementation, so libolm is not needed.
  tags = [ "goolm" ];
  subPackages = [ "cmd/mautrix-chatto" ];

  ldflags = [
    "-s"
    "-w"
    "-X main.Tag=v${version}"
    "-X main.Commit=${rev}"
  ];

  # Tests run as a separate flake check so that the package builds fast.
  doCheck = false;

  meta = {
    description = "A Matrix-Chatto puppeting bridge";
    homepage = "https://github.com/elsbrock/matrix-chatto-bridge";
    license = lib.licenses.agpl3Plus;
    mainProgram = "mautrix-chatto";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
