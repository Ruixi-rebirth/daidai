{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      perSystem = flake-utils.lib.eachDefaultSystem (system:
        let
          pkgs = import nixpkgs { inherit system; };
          fontConfig = pkgs.makeFontsConf {
            fontDirectories = with pkgs; [
              maple-mono.CN
              source-han-sans
              twemoji-color-font
            ];
          };
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
              ffmpeg
              chromium
              wgcf
              wireproxy
              maple-mono.CN
              source-han-sans
              twemoji-color-font
            ];
            shellHook = ''
              export CGO_ENABLED=0
              export FONTCONFIG_FILE="${fontConfig}"
              export TYPST_FONT_PATHS="${pkgs.twemoji-color-font}/share/fonts/truetype:${pkgs.maple-mono.CN}/share/fonts/truetype:${pkgs.source-han-sans}/share/fonts/truetype"
              echo "✨ Daidai Development Environment Loaded ✨"
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
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.typst pkgs.ffmpeg pkgs.chromium pkgs.wgcf pkgs.wireproxy ]} \
                --set-default FONTCONFIG_FILE ${fontConfig} \
                --set-default TYPST_FONT_PATHS "${pkgs.twemoji-color-font}/share/fonts/truetype:${pkgs.maple-mono.CN}/share/fonts/truetype:${pkgs.source-han-sans}/share/fonts/truetype"
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
