{
  lib,
  config,
  pkgs,
  ...
}:
let
  cfg = config.services.mautrix-chatto;
  dataDir = "/var/lib/mautrix-chatto";
  registrationFile = "${dataDir}/chatto-registration.yaml";
  settingsFile = "${dataDir}/config.yaml";
  settingsFormat = pkgs.formats.json { };
  settingsFileUnsubstituted = settingsFormat.generate "mautrix-chatto-config-unsubstituted.json" cfg.settings;

  optOneOf = lib.lists.findFirst (value: value.condition) (lib.mkIf false null);
  mkDefaults = lib.mapAttrsRecursive (_: v: lib.mkDefault v);

  defaultConfig = {
    network = {
      server_url = "";
      displayname_template = "{{or .DisplayName .Login}}{{if .IsBot}} (bot){{end}}";
      sync_presence = true;
      keep_logins_online = false;
      call_notices = true;
      thread_echo = "skip";
      allow_password_login = true;
      strict_server_version = false;
      metrics = {
        enabled = cfg.metrics.enable;
        listen = "${cfg.metrics.listenAddress}:${toString cfg.metrics.port}";
      };
    };
    bridge = {
      command_prefix = "!chatto";
      permissions."*" = "relay";
      relay.enabled = true;
    };
    database = {
      type = "sqlite3-fk-wal";
      uri = "file:${dataDir}/mautrix-chatto.db?_txlock=immediate";
    };
    homeserver.address = "http://localhost:8448";
    appservice = {
      hostname = "127.0.0.1";
      port = 29340;
      id = "chatto";
      bot = {
        username = "chattobot";
        displayname = "Chatto Bridge Bot";
      };
      as_token = "";
      hs_token = "";
      username_template = "chatto_{{.}}";
    };
    double_puppet = {
      servers = { };
      secrets = { };
    };
    # These default to "generate", which would rotate them on every restart.
    encryption.pickle_key = "";
    provisioning.shared_secret = "";
    public_media.signing_key = "";
    direct_media.server_key = "";
    backfill.enabled = true;
    logging = {
      min_level = "info";
      writers = lib.singleton {
        type = "stdout";
        format = "json";
      };
    };
  };
in
{
  options.services.mautrix-chatto = {
    enable = lib.mkEnableOption "mautrix-chatto, a Matrix-Chatto puppeting bridge";

    package = lib.mkPackageOption pkgs "mautrix-chatto" { };

    settings = lib.mkOption {
      apply = lib.recursiveUpdate defaultConfig;
      type = settingsFormat.type;
      default = defaultConfig;
      description = ''
        {file}`config.yaml` as a Nix attribute set, merged over the module's
        defaults. See `mautrix-chatto -e -c example.yaml` for every option.
        Put secrets in {option}`environmentFile` and reference them as
        `$VARIABLE`; the file is rendered with envsubst at startup.
      '';
      example = lib.literalExpression ''
        {
          network.server_url = "https://chat.example.com";
          homeserver = {
            address = "http://[::1]:8008";
            domain = "example.com";
          };
          bridge.permissions = {
            "example.com" = "user";
            "@admin:example.com" = "admin";
          };
          encryption = {
            allow = true;
            default = true;
            pickle_key = "$MAUTRIX_CHATTO_PICKLE_KEY";
          };
        }
      '';
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        File with environment variables for the service, e.g. secrets
        referenced from {option}`settings`. If `MAUTRIX_CHATTO_DOUBLE_PUPPET_SECRET`
        is set, it is used as the double puppeting secret for the configured
        homeserver domain.
      '';
    };

    metrics = {
      enable = lib.mkEnableOption "the Prometheus metrics and health endpoints";
      listenAddress = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1";
        description = "Address the metrics endpoint listens on.";
      };
      port = lib.mkOption {
        type = lib.types.port;
        default = 8001;
        description = "Port of the metrics endpoint (`/metrics`, `/healthz`, `/readyz`).";
      };
      addScrapeConfig = lib.mkOption {
        type = lib.types.bool;
        default = cfg.metrics.enable && config.services.prometheus.enable;
        defaultText = lib.literalExpression "config.services.mautrix-chatto.metrics.enable && config.services.prometheus.enable";
        description = "Whether to add a scrape job for the bridge to the local Prometheus.";
      };
    };

    serviceDependencies = lib.mkOption {
      type = with lib.types; listOf str;
      default =
        (lib.optional config.services.matrix-synapse.enable config.services.matrix-synapse.serviceUnit)
        ++ (lib.optional config.services.matrix-conduit.enable "conduit.service")
        ++ (lib.optional (config.services ? matrix-tuwunel && config.services.matrix-tuwunel.enable) "tuwunel.service");
      defaultText = lib.literalExpression ''
        (optional config.services.matrix-synapse.enable config.services.matrix-synapse.serviceUnit)
        ++ (optional config.services.matrix-conduit.enable "conduit.service")
        ++ (optional config.services.matrix-tuwunel.enable "tuwunel.service")
      '';
      description = "Systemd units to require and wait for before starting the bridge.";
    };

    registerToSynapse = lib.mkOption {
      type = lib.types.bool;
      default = config.services.matrix-synapse.enable;
      defaultText = lib.literalExpression "config.services.matrix-synapse.enable";
      description = ''
        Whether to add the registration file to
        `services.matrix-synapse.settings.app_service_config_files`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.settings.network.server_url != "";
        message = "services.mautrix-chatto.settings.network.server_url must be set to your Chatto server.";
      }
      {
        assertion = cfg.settings.homeserver.domain or "" != "";
        message = "services.mautrix-chatto.settings.homeserver.domain must be set.";
      }
    ];

    users.users.mautrix-chatto = {
      isSystemUser = true;
      group = "mautrix-chatto";
      home = dataDir;
      description = "mautrix-chatto bridge user";
    };
    users.groups.mautrix-chatto = { };

    services.matrix-synapse = lib.mkIf cfg.registerToSynapse {
      settings.app_service_config_files = [ registrationFile ];
    };
    systemd.services.matrix-synapse = lib.mkIf cfg.registerToSynapse {
      serviceConfig.SupplementaryGroups = [ "mautrix-chatto" ];
    };

    services.mautrix-chatto.settings.homeserver = optOneOf (
      with config.services;
      [
        (lib.mkIf matrix-synapse.enable (mkDefaults {
          domain = matrix-synapse.settings.server_name;
        }))
        (lib.mkIf matrix-conduit.enable (mkDefaults {
          domain = matrix-conduit.settings.global.server_name;
          address = "http://localhost:${toString matrix-conduit.settings.global.port}";
        }))
      ]
    );

    services.prometheus.scrapeConfigs = lib.mkIf cfg.metrics.addScrapeConfig [
      {
        job_name = "mautrix-chatto";
        static_configs = [
          { targets = [ "${cfg.metrics.listenAddress}:${toString cfg.metrics.port}" ]; }
        ];
      }
    ];

    systemd.services.mautrix-chatto = {
      description = "mautrix-chatto, a Matrix-Chatto puppeting bridge";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ] ++ cfg.serviceDependencies;
      after = [ "network-online.target" ] ++ cfg.serviceDependencies;

      preStart = ''
        test -f '${settingsFile}' && rm -f '${settingsFile}'
        old_umask=$(umask)
        umask 0177
        ${pkgs.envsubst}/bin/envsubst -o '${settingsFile}' -i '${settingsFileUnsubstituted}'
        umask $old_umask

        if [ ! -f '${registrationFile}' ]; then
          ${lib.getExe cfg.package} \
            --generate-registration \
            --config='${settingsFile}' \
            --registration='${registrationFile}'
        fi
        chmod 640 '${registrationFile}'

        umask 0177
        ${pkgs.yq}/bin/yq -s '.[0].appservice.as_token = .[1].as_token
          | .[0].appservice.hs_token = .[1].hs_token
          | .[0]
          | if env.MAUTRIX_CHATTO_DOUBLE_PUPPET_SECRET then .double_puppet.secrets.[.homeserver.domain] = env.MAUTRIX_CHATTO_DOUBLE_PUPPET_SECRET else . end' \
          '${settingsFile}' '${registrationFile}' > '${settingsFile}.tmp'
        mv '${settingsFile}.tmp' '${settingsFile}'
        umask $old_umask
      '';

      serviceConfig = {
        Type = "simple";
        User = "mautrix-chatto";
        Group = "mautrix-chatto";
        EnvironmentFile = lib.mkIf (cfg.environmentFile != null) cfg.environmentFile;
        StateDirectory = baseNameOf dataDir;
        StateDirectoryMode = "0750";
        WorkingDirectory = dataDir;
        ExecStart = "${lib.getExe cfg.package} --config='${settingsFile}' --registration='${registrationFile}' --no-update";
        Restart = "on-failure";
        RestartSec = "30s";

        CapabilityBoundingSet = "";
        DevicePolicy = "closed";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        PrivateUsers = true;
        ProcSubset = "pid";
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RemoveIPC = true;
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        SystemCallArchitectures = "native";
        SystemCallErrorNumber = "EPERM";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        UMask = "0027";
      };
      restartTriggers = [ settingsFileUnsubstituted ];
    };
  };
}
