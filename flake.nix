# flake.nix — todoku-go (GSDS Biblioteca) via substrate's go-library-flake.
#
# The substrate Go builders are import-PATHS returning WHOLE-FLAKE outputs
# (packages / devShells / apps / overlays). They are called in two stages at the
# TOP LEVEL — first `import <path> { inherit nixpkgs; }`, then the spec attrset
# `{ name; src; … }`. There is NO flake-parts `perSystem` wrapper; the builder
# fans out across systems internally.
#
# packages.default = mkGoLibraryCheck → `go build ./...` in the Nix sandbox.
# vendorHash is OMITTED → spec-sourced (__from-spec__); the clean nix build
# lands once the module graph is fully published. Pre-publish proof is
# `GOTOOLCHAIN=local go test ./...` (green). todoku-go depends only on
# golang.org/x/{net,time} (published Go-team modules), confined to the
# todoku/h2 and todoku/budget leaf sub-packages (BOREALIS Law 6).
{
  description = "todoku-go — pleme-io's standard outbound-HTTP client for Go (Biblioteca)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    substrate = {
      # LOCAL verification points at the feat branch carrying the Go builders.
      # The PUBLISHED repo uses: url = "github:pleme-io/substrate";
      url = "git+file:///Users/drzzln/code/github/pleme-io/substrate?ref=feat/go-pattern-parity";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs @ { self, nixpkgs, substrate, ... }:
    (import substrate.goLibraryFlakeBuilder { inherit nixpkgs; }) {
      name = "todoku-go";
      version = "0.3.0";
      src = self;
      repo = "pleme-io/todoku-go";
    };
}
