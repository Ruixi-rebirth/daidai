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
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "Daidai 软件包。";
    };

    configFile = lib.mkOption {
      type = lib.types.str;
      description = "Daidai 配置文件的运行时路径；敏感配置应从 /run/secrets 等运行时秘密目录读取，不要放入 Nix store。";
    };
  };

  config = lib.mkIf cfg.enable {
    users.groups.daidai = { };
    users.users.daidai = {
      isSystemUser = true;
      group = "daidai";
      home = "/var/lib/daidai";
      createHome = true;
    };

    systemd.services.daidai = {
      description = "Daidai Super Digital Lifeform Bot";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      path = [ config.system.path ];
      serviceConfig = {
        StateDirectory = "daidai";
        StateDirectoryMode = "0700";
        WorkingDirectory = "/var/lib/daidai";
        User = lib.mkDefault "daidai";
        Group = lib.mkDefault "daidai";
        UMask = "0077";
        ExecStart = "${cfg.package}/bin/daidai -config ${cfg.configFile}";
        Restart = "always";
        RestartSec = "5s";
        TimeoutStopSec = "5min";
      };
    };
  };
}
