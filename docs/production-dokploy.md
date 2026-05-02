# Dokploy And Coolify Setup

This guide covers DPS on Docker hosts managed by Dokploy, Coolify, or a similar Compose-based PaaS.

DPS is installed on the Docker host. Once installed, any Compose project on that host can request a limited volume with `driver: dps`.

## How DPS Mounts Volumes

```text
Compose / PaaS
  -> Docker creates a named volume with driver: dps
  -> DPS creates /var/lib/dps/volume-images/<volume>.img
  -> DPS formats it as ext4 with the requested inode count
  -> DPS mounts it at /mnt/dps/volumes/<volume>
  -> DPS returns /mnt/dps/volumes/<volume>/data to Docker
  -> Docker bind-mounts that clean data directory into the container
```

The extra `data` directory is part of the design. It hides filesystem metadata such as `lost+found` from applications, while keeping the container inside the same limited filesystem.

## Install On Ubuntu 24.04 arm64

Run this on each Docker host that should support DPS volumes:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

Defaults:

- `DPS_ROOT=/var/lib/dps`
- `DPS_IMAGE_ROOT=/var/lib/dps/volume-images`
- `DPS_MOUNT_ROOT=/mnt/dps`
- `DPS_DEFAULT_VOLUME_SIZE=5G`
- `DPS_DEFAULT_VOLUME_INODES=200000`
- `DPS_ARCHIVE_POLICY=offline`

The installer prints a visible success/failure summary and creates a small test volume before reporting success.

To place volume image files on another disk or directory:

```sh
sudo env DPS_IMAGE_ROOT=/srv/dps-images bash install-dps.sh
```

To change the default volume size:

```sh
sudo env DPS_DEFAULT_VOLUME_SIZE=2G bash install-dps.sh
```

## Validate The Service

```sh
systemctl status dpsd --no-pager --full
journalctl -u dpsd -n 100 --no-pager
cat /etc/dps/dpsd.env
```

Expected config shape:

```text
DPS_ROOT=/var/lib/dps
DPS_MOUNT_ROOT=/mnt/dps
DPS_IMAGE_ROOT=/var/lib/dps/volume-images
```

## Compose Example

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: example
      POSTGRES_DB: app
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
    driver: dps
    driver_opts:
      size: 5G
      inodes: "500000"
```

For smaller apps:

```yaml
volumes:
  appdata:
    driver: dps
    driver_opts:
      size: 2G
      inodes: "50000"
```

If defaults are acceptable:

```yaml
volumes:
  appdata:
    driver: dps
```

## Validate Limits

PaaS tools usually prefix the real Docker volume name with the project/app name.

```sh
docker volume ls | grep pgdata
docker volume inspect <real-volume-name>
```

Check from inside the container:

```sh
docker ps
docker exec -it <container> df -h /var/lib/postgresql/data
docker exec -it <container> df -i /var/lib/postgresql/data
```

Expected shape:

```text
Filesystem      Size  Used Avail Use% Mounted on
/dev/loopX       5G   ...   ...   ... /var/lib/postgresql/data

Filesystem     Inodes IUsed IFree IUse% Mounted on
/dev/loopX      500000 ...   ...   ... /var/lib/postgresql/data
```

`/dev/loopX` is expected. Linux is presenting the DPS image file as a mounted block device.

## Existing Volumes

Docker does not convert an existing volume from `local` to `dps`. If an app was deployed before adding DPS:

1. Stop/remove the app through Dokploy/Coolify.
2. Back up or export existing data if it matters.
3. Remove or rename the old Docker volume.
4. Redeploy with `driver: dps`.

For disposable test data:

```sh
docker volume rm <real-volume-name>
```

## Resize

Resize is offline. Stop the app in the PaaS UI first.

```sh
dpsctl resize <volume> 10G 800000
```

Behavior:

- increasing size grows the existing image when possible;
- decreasing size recreates the image and restores data;
- changing inode count recreates the filesystem;
- shrink is refused unless current usage fits with 10% headroom;
- mounted volumes are refused.

## Snapshots And Backups

Default policy is `offline`, so `snapshot` and `backup-volume` are refused while Docker still has active references to the volume.

```sh
dpsctl snapshot pgdata before-upgrade
dpsctl backup before-upgrade /srv/dps-backups
dpsctl backup-volume pgdata s3://bucket/prod pgdata-manual
```

For databases, stop writes first or use tested hooks:

```sh
dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  backup-volume pgdata s3://bucket/prod pgdata-hooked
```

## Uninstall DPS

The uninstall script removes DPS software and integration points by default, but preserves apps, containers, Docker volumes, and DPS volume image data.

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/uninstall-dps-host.sh -o uninstall-dps-host.sh
sudo bash uninstall-dps-host.sh
```

Non-interactive:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps bash uninstall-dps-host.sh
```

Optional data removal:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_REMOVE_DATA=true bash uninstall-dps-host.sh
```

Optional Docker volume metadata removal:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES=true bash uninstall-dps-host.sh
```

## Diagnose Deploy Failures

When the PaaS UI reports only a generic Compose failure, collect host diagnostics:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/diagnose-dokploy-dps.sh -o diagnose-dokploy-dps.sh
sudo bash diagnose-dokploy-dps.sh
```

Optional DPS driver test:

```sh
sudo env DPS_DIAG_RUN_VOLUME_TEST=true bash diagnose-dokploy-dps.sh
```

Common causes:

- Docker daemon unhealthy;
- app still using an existing `local` volume;
- volume referenced by a stopped/old container;
- app container failing after volume creation;
- DPS service/socket/mount issue.

## Managed Plugin Alternative

```sh
make plugin-rootfs
sudo docker plugin create dps:latest packaging/docker-plugin
sudo docker plugin set dps:latest DPS_DEFAULT_VOLUME_SIZE=5G
sudo docker plugin set dps:latest DPS_DEFAULT_VOLUME_INODES=200000
sudo docker plugin set dps:latest DPS_ARCHIVE_POLICY=offline
sudo docker plugin enable dps:latest
```

For production PaaS hosts, the unmanaged systemd service is recommended because it is easier to observe, restart, and debug.
