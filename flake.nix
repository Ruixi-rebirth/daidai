{
  description = "Daidai Bot - Golang Refactor with Nix";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      perSystem = flake-utils.lib.eachDefaultSystem (system:
        let
          pkgs = import nixpkgs { inherit system; };
        in {
          devShells.default = pkgs.mkShell {
            name = "daidai-dev-shell";
            buildInputs = with pkgs; [
              go
              gopls
              golangci-lint
              mermaid-cli
              just
              sqlite
              curl
              jq
              typst
              chromium
              wgcf
              wireproxy
              maple-mono.CN
              source-han-sans
            ];
            shellHook = ''
              export CGO_ENABLED=0
              export TYPST_FONT_PATHS="${pkgs.maple-mono.CN}/share/fonts/truetype:${pkgs.source-han-sans}/share/fonts/truetype"
              echo "✨ Daidai Go Refactor Environment Loaded ✨"
              echo "Go version: $(go version)"
              echo "Typst version: $(typst --version)"
            '';
          };

          packages.default = pkgs.buildGoModule {
            pname = "daidai";
            version = "0.2.0";
            src = ./.;
            vendorHash = null;
            env.CGO_ENABLED = 0;
            subPackages = [ "cmd/daidai" ];
            nativeBuildInputs = [ pkgs.makeWrapper ];
            postInstall = ''
              wrapProgram $out/bin/daidai \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.typst pkgs.chromium pkgs.wgcf pkgs.wireproxy ]} \
                --set-default TYPST_FONT_PATHS "${pkgs.maple-mono.CN}/share/fonts/truetype:${pkgs.source-han-sans}/share/fonts/truetype"
            '';
          };
          packages.daidai = self.packages.${system}.default;
        }
      );
    in
    perSystem // {
      nixosModules.default = import ./nix/module.nix self;
      nixosModules.daidai = self.nixosModules.default;
    };
}
