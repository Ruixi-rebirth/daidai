self:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.daidai;
in
{
  options.services.daidai = {
    enable = lib.mkEnableOption "Daidai Super Digital Lifeform Bot";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.system}.default;
      description = "Daidai 软件包。";
    };

    configFile = lib.mkOption {
      type = lib.types.path;
      description = "Daidai 配置文件路径。";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.daidai = {
      description = "Daidai Super Digital Lifeform Bot";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        StateDirectory = "daidai";
        WorkingDirectory = "/var/lib/daidai";
        ExecStart = "${cfg.package}/bin/daidai -config ${cfg.configFile}";
        Restart = "always";
        RestartSec = "5s";
      };
    };
  };
}
