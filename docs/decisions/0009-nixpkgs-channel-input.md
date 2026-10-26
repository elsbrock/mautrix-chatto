# 0009: nixpkgs flake input via channel tarball

Status: accepted (2026-10-26)

## Context
The session that built this had no GitHub API access, so `github:NixOS/nixpkgs`
could not be locked.

## Decision
Use `https://channels.nixos.org/nixos-unstable/nixexprs.tar.xz`. Nix resolves it
to an immutable `releases.nixos.org` URL in `flake.lock`, so this is as
reproducible as a `github:` input. Users who prefer `github:` can override the
input in their own flake (`inputs.mautrix-chatto.inputs.nixpkgs.follows`).
