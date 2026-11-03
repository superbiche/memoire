# Deploy

Local dev and smoke use `podman compose` with a named volume mounted at `/data`.

```bash
podman compose up --build
```

The service binds to `127.0.0.1:37778` on the host.

## Local Always-On Service

The local always-on path uses a rootless Podman Quadlet unit. `compose.yml` stays the dev/smoke surface; Quadlet is the local user-session supervisor for the same container image.

```bash
make quadlet-start
```

This target builds `localhost/memoire:latest`, installs `deploy/quadlet/memoire.container` to `~/.config/containers/systemd/memoire.container`, starts the generated `memoire.service`, and waits for `scripts/check-health.sh`.

After code or schema changes, rebuild and restart the local service with:

```bash
make quadlet-rebuild
```

This builds `localhost/memoire:latest`, reinstalls the Quadlet unit, restarts
`memoire.service`, waits for readiness, and reports operational health
threshold failures. The direct script entry point is
`./scripts/quadlet-rebuild.sh`.

Useful operations:

```bash
make quadlet-status
make quadlet-health
make quadlet-stop
./scripts/quadlet.sh uninstall
```

The Quadlet unit mounts `~/.memoire` into the container as `/data`, listens on all interfaces at port 37778 (published as `37778:37778`, so the service is reachable from the LAN; bearer auth is the trust boundary and the host firewall the outer wall), and uses `Restart=always`. The unit includes `[Install] WantedBy=default.target`; Podman's Quadlet generator applies this install section when user units are reloaded.

The above covers the local single-machine service. The maintainers' shared hub deployment (Kubernetes manifests, image pins, deploy gates) lives in their private ledger.
