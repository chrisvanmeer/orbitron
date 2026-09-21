# Orbitron - Internal Ansible Galaxy Mirror Daemon
#
# Deploy with:
#   nomad job validate orbitron.nomad.hcl
#   nomad job plan   orbitron.nomad.hcl
#   nomad job run    orbitron.nomad.hcl
#
# REQUIREMENTS
#   - A Docker-capable Nomad client (docker driver enabled).
#   - A registered host volume named "orbitron-data" on the target client to
#     persist the mirror cache and admin token. Register it in the client agent
#     configuration, e.g.:
#
#       client {
#         host_volume "orbitron-data" {
#           path      = "/srv/orbitron-data"
#           read_only = false
#         }
#       }
#
#   - The GHCR image is public; no registry credentials are required.

job "orbitron" {
  # Target datacenter(s) for this job.
  datacenters = ["dc1"]
  type        = "service"

  # Rolling update strategy.
  update {
    max_parallel      = 1
    min_healthy_time  = "10s"
    healthy_deadline  = "3m"
    progress_deadline = "10m"
    auto_revert       = true
  }

  group "orbitron" {
    # Number of daemon instances; keep at 1 to avoid mirror races on the
    # shared volume unless you have investigated the implications.
    count = 1

    # Allow the load balancer / proxy to deregister before the container stops.
    shutdown_delay = "15s"

    # Restart policy for task failures.
    restart {
      attempts = 2
      interval = "30m"
      delay    = "15s"
      mode     = "fail"
    }

    # Persist the mirror storage across redeploys. The "orbitron-data" host
    # volume must be registered on the client (see header comment).
    volume "orbitron-data" {
      type      = "host"
      source    = "orbitron-data"
      read_only = false
    }

    network {
      # Map an external (host-allocated) port to the internal container port.
      port "http" {
        to = 8080
      }
    }

    # Service registration for Nomad service discovery / Consul.
    service {
      name     = "orbitron"
      port     = "http"
      provider = "nomad" # Set to "consul" when using HashiCorp Consul

      # Optional Traefik reverse proxy routing (uncomment to enable):
      # tags = [
      #   "traefik.enable=true",
      #   "traefik.http.routers.orbitron.entrypoints=https",
      #   "traefik.http.routers.orbitron.rule=Host(`orbitron.example.com`)",
      # ]

      check {
        name     = "orbitron healthz"
        type     = "http"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "orbitron" {
      driver = "docker"

      config {
        image = "ghcr.io/chrisvanmeer/orbitron:latest"

        # Always pull the latest image tag upon deployment/restart.
        force_pull = true

        ports = ["http"]
      }

      volume_mount {
        volume      = "orbitron-data"
        destination = "/data"
        read_only   = false
      }

      env {
        ORBITRON_LISTEN_ADDR       = "0.0.0.0:8080"
        ORBITRON_STORAGE_PATH      = "/data"
        # File logs land on the persistent host volume so the SYSTEM LOGS
        # panel in the web UI works. Use "local/orbitron.log" for ephemeral
        # logs tied to the alloc dir, or "" to log only to stdout
        # (nomad alloc logs <alloc-id>; the UI then shows a stdout hint).
        ORBITRON_LOG_PATH          = "/data/orbitron.log"
        ORBITRON_TOKENS_FILE       = "/data/tokens.json"
        ORBITRON_REQUIRE_AUTH_PULL = "false"
        ORBITRON_MAX_CONCURRENCY   = "4"
        ORBITRON_TOKEN_TTL_DAYS    = "0"
        ORBITRON_AUTO_TOKEN        = "true"
        TZ                         = "Europe/Amsterdam"
        # Optional forward proxy for outbound sync (e.g. Squid):
        # ORBITRON_HTTP_PROXY  = "http://proxy.internal:3128"
        # ORBITRON_HTTPS_PROXY = "http://proxy.internal:3128"
        # ORBITRON_NO_PROXY    = "localhost,127.0.0.1,10.0.0.0/8"
        # Optional SSO (OIDC / Keycloak) authentication for the web dashboard.
        # When enabled, the /ui login page additionally offers "SSO Login"
        # next to the regular access-token login. Keep the client secret in
        # Nomad Variables or Vault (see Option A/B below) rather than inline.
        # ORBITRON_OIDC_ENABLED        = "true"
        # ORBITRON_OIDC_ISSUER         = "https://keycloak.example.org/realms/orbitron"
        # ORBITRON_OIDC_CLIENT_ID      = "orbitron"
        # ORBITRON_OIDC_CLIENT_SECRET  = "<client secret from Keycloak>"
        # ORBITRON_OIDC_SESSION_TTL_HOURS = "8"
        # Leave empty to auto-derive the redirect URI from the request as
        # <scheme>://<host>/ui/oidc/callback; when set explicitly it must match
        # a Keycloak-registered redirect URI.
        # ORBITRON_OIDC_REDIRECT_URI  = ""
        # Optionally restrict login to members of these Keycloak groups
        # (comma-separated; requires the ID token "groups" claim, see README).
        # ORBITRON_OIDC_ALLOWED_GROUPS = "orbitron-admins,orbitron-ops"
      }

      # --- Option A: Nomad Variables (default: token auto-generated) ----------
      # The container generates an admin token on first start and stores it at
      # /data/tokens.json (survives restarts thanks to the host volume). The
      # token is printed once to the container logs:
      #   nomad alloc logs <alloc-id> | grep "ADMIN TOKEN"
      #
      # To keep secrets out of logs entirely, disable auto-generation and stage
      # a pre-made token file via a template, e.g.:
      #
      # template {
      #   data = <<EOH
      # {{- with nomadVar "nomad/jobs/orbitron" -}}
      # {"tokens": {{ .tokensJson }}}
      # {{- end -}}
      # EOH
      #   destination = "local/tokens.json"
      #   perms       = "0600"
      # }
      #
      # and point ORBITRON_TOKENS_FILE at the rendered file.

      # --- Option B: Vault Integration via Workload Identity -----------------
      # Uncomment to pull configuration and secrets from Vault KV v2 instead
      # of the env block above:
      #
      # vault {
      #   role = "orbitron" # Vault role configured for Nomad Workload Identity
      # }
      #
      # template {
      #   data = <<EOH
      # {{- with secret "secret/data/orbitron" -}}
      # ORBITRON_LISTEN_ADDR="{{ .Data.data.orbitron_listen_addr }}"
      # ORBITRON_TOKENS_FILE="{{ .Data.data.orbitron_tokens_file }}"
      # {{- end -}}
      # EOH
      #   destination = "secrets/env"
      #   env         = true
      # }

      resources {
        cpu    = 200 # MHz limit
        memory = 128 # Memory limit in MB (git syncs and index parsing need headroom)
      }
    }
  }
}
