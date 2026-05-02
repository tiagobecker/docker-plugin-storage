# DPS - Docker Plugin Storage

DPS is a Docker volume driver for local persistent volumes with per-volume storage and inode limits. It is designed for ordinary Linux Docker hosts and PaaS-style environments such as Dokploy and Coolify, where users need simple Compose syntax and predictable disk boundaries for application volumes.

DPS focuses on a practical problem Docker does not solve well by default: limiting how much storage a named volume can consume. It also includes the administration tools expected around that feature: validation through `df`, offline resize up/down, local snapshots, local backups, S3-compatible backups, restore, and database-aware archive policies.

## Status

DPS is built as a functional Docker storage plugin for real Docker hosts. The primary production path is the unmanaged systemd service, because it is simple to install, easy to inspect, and works directly with the host's Linux mount and loop device facilities.

The storage backend is intentionally conservative: one ext4 image per volume. This keeps deployment simple across common Linux distributions while still providing clear storage and inode boundaries for Compose-managed applications.

## Storage Model

DPS uses one portable backend:

```text
Docker volume
  -> DPS creates one ext4 filesystem image file
  -> DPS mounts that image through a loop device
  -> DPS returns a clean data subdirectory to Docker
  -> Docker bind-mounts that path into the container
```

Example path layout:

```text
/var/lib/dps/volume-images/<volume>.img   # real volume data image
/mnt/dps/volumes/<volume>                 # internal ext4 mount
/mnt/dps/volumes/<volume>/data            # path returned to Docker
```

The `data` subdirectory is intentional. ext filesystems create metadata such as `lost+found` at the filesystem root; DPS keeps that internal so applications see a clean volume directory. This avoids first-boot failures in apps and databases that require an empty data directory.

The container will usually show the volume filesystem as `/dev/loopX`. That is expected: Linux is exposing the volume image file as a block device.

## Features

- Docker `VolumeDriver` API over `/run/docker/plugins/dps.sock`.
- Per-volume size limit visible with `df -h` inside the container.
- Per-volume inode limit visible with `df -i` inside the container.
- Simple Docker Compose / Dokploy / Coolify syntax using `driver_opts`.
- Configurable global defaults for size and inodes.
- Offline resize up and down with data-fit checks.
- Local snapshots with manifest, byte count, and SHA-256 verification.
- Local and S3-compatible backups with manifest, checksum, and verification.
- Restore from snapshots and backups into stopped volumes.
- Archive consistency policies: `offline`, `crash-consistent`, and `hooked`.
- Conservative uninstall flow that does not remove Docker volumes or app deploys by default.

## Compose Usage

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
    driver: dps
    driver_opts:
      size: 5G
      inodes: "500000"
```

If the host defaults are acceptable, `driver_opts` can be omitted:

```yaml
volumes:
  appdata:
    driver: dps
```

Validate from inside the container:

```sh
docker exec -it <container> df -h /path/to/volume
docker exec -it <container> df -i /path/to/volume
```

Expected shape:

```text
Filesystem      Size  Used Avail Use% Mounted on
/dev/loopX      5.0G  ...  ...   ...  /path/to/volume

Filesystem     Inodes IUsed IFree IUse% Mounted on
/dev/loopX      500000 ...   ...   ... /path/to/volume
```

Small inode differences are normal because `mkfs.ext4` may round the requested inode count to a valid filesystem layout.

## Install On Ubuntu 24.04 arm64

This installer is intended for Linux hosts where Docker is already installed, including servers managed by Dokploy or Coolify.

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

The installer:

- checks Ubuntu 24.04 and arm64/aarch64;
- verifies Docker is reachable;
- refuses conflicting managed plugins named like `dps`;
- installs required host packages: `ca-certificates`, `e2fsprogs`, `git`, `util-linux`;
- builds `dpsd` and `dpsctl` for linux/arm64;
- installs a systemd service named `dpsd`;
- writes `/etc/dps/dpsd.env`;
- starts DPS and creates a real test volume;
- prints a clear success or failure summary.

Defaults:

```text
DPS_ROOT=/var/lib/dps
DPS_IMAGE_ROOT=/var/lib/dps/volume-images
DPS_MOUNT_ROOT=/mnt/dps
DPS_DEFAULT_VOLUME_SIZE=5G
DPS_DEFAULT_VOLUME_INODES=200000
DPS_ARCHIVE_POLICY=offline
```

To place volume image files on another disk or directory:

```sh
sudo env DPS_IMAGE_ROOT=/srv/dps-images bash install-dps.sh
```

To change the host-wide default size:

```sh
sudo env DPS_DEFAULT_VOLUME_SIZE=2G bash install-dps.sh
```

## PaaS Notes: Dokploy And Coolify

DPS works at the Docker host level. Install it on every Docker host where Compose projects should be able to use `driver: dps`.

For Dokploy or Coolify templates, keep the Compose volume section explicit:

```yaml
volumes:
  app-data:
    driver: dps
    driver_opts:
      size: 2G
      inodes: "50000"
```

If a project was previously deployed with Docker's default `local` driver, Docker will not convert that existing volume to DPS. Stop/remove the app through the PaaS UI, remove or rename the old volume if the data is disposable, then redeploy with `driver: dps`.

## Host Service

Build locally:

```sh
make build
```

Run as an unmanaged Docker volume plugin:

```sh
sudo ./bin/dpsd \
  --root /var/lib/dps \
  --mount-root /mnt/dps \
  --image-root /var/lib/dps/volume-images \
  --default-volume-size 5G \
  --default-volume-inodes 200000 \
  --socket /run/docker/plugins/dps.sock
```

Important paths:

- `--root`: metadata, snapshots, temporary files.
- `--image-root`: ext4 image files that hold real volume data.
- `--mount-root`: internal mount root; Docker receives `<mount-root>/volumes/<volume>/data`.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DPS_ROOT` | `/var/lib/dps` | Metadata, snapshots, temporary files. |
| `DPS_IMAGE_ROOT` | `<DPS_ROOT>/volume-images` | Volume image files. Put this on the storage path where data should live. |
| `DPS_MOUNT_ROOT` | `/mnt/dps` | Internal mount root. Docker receives each volume's `data` subdirectory. |
| `DPS_DEFAULT_VOLUME_SIZE` | `5G` | Default size when Compose omits `driver_opts.size`. |
| `DPS_DEFAULT_VOLUME_INODES` | `200000` | Default inode count when Compose omits `driver_opts.inodes`. |
| `DPS_ARCHIVE_POLICY` | `offline` | Snapshot/backup consistency policy. |
| `DPS_PRE_ARCHIVE_HOOK` | empty | Hook command for `hooked` policy. |
| `DPS_POST_ARCHIVE_HOOK` | empty | Hook command for `hooked` policy. |
| `DPS_ARCHIVE_HOOK_TIMEOUT` | `10m` | Hook timeout. |
| `DPS_SOCKET` | `/run/docker/plugins/dps.sock` | Docker plugin socket. |

## Resize

Resize is offline. Stop the workload first or ensure Docker has released the volume.

```sh
dpsctl resize pgdata 10G 800000
```

Behavior:

- increasing size grows the ext4 image when possible;
- decreasing size recreates the image and restores data;
- changing inode count recreates the filesystem because ext4 inode count is fixed at creation;
- shrink operations are refused unless current usage fits with at least 10% headroom;
- mounted volumes are refused for resize.

## Snapshots

Create a local snapshot:

```sh
dpsctl snapshot pgdata before-upgrade
```

Restore:

```sh
dpsctl restore before-upgrade pgdata
```

Snapshots are `tar.gz` archives of the volume data directory. DPS writes a manifest, records byte count and SHA-256, verifies before restore, and refuses restore into mounted volumes.

## Backups

Back up a snapshot locally:

```sh
dpsctl backup before-upgrade /srv/dps-backups
```

Stream a volume backup without first writing a snapshot file:

```sh
dpsctl backup-volume pgdata /srv/dps-backups pgdata-manual
```

S3-compatible target:

```sh
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1

dpsctl backup-volume pgdata s3://bucket/prod pgdata-manual
dpsctl backup-verify s3://bucket/prod pgdata-manual
```

For MinIO or another compatible endpoint:

```sh
export AWS_ENDPOINT_URL=https://minio.example.com
export AWS_S3_FORCE_PATH_STYLE=true
```

Backups include manifests and checksums. Verification reads the backup payload and compares it with the recorded manifest.

## Data Consistency

The default policy is `offline`: DPS refuses `snapshot` and `backup-volume` while the volume has active Docker references. This is the safest default for databases and stateful services.

Recommended flows:

- Stop the app in Dokploy/Coolify, run snapshot or backup, then start it again.
- Use `DPS_ARCHIVE_POLICY=hooked` with tested pre/post hooks that quiesce the database.
- Keep logical database backups in addition to DPS volume backups for critical production databases.

Mounted capture is explicit:

```sh
dpsctl --archive-policy crash-consistent backup-volume pgdata /srv/dps-backups pgdata-crash
```

Hooked capture:

```sh
dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  backup-volume pgdata s3://bucket/prod pgdata-hooked
```

## Uninstall

The uninstall script removes DPS software and integration points while preserving Dokploy/Coolify apps, containers, Docker volumes, and DPS image data by default.

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/uninstall-dps-host.sh -o uninstall-dps-host.sh
sudo bash uninstall-dps-host.sh
```

Non-interactive:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps bash uninstall-dps-host.sh
```

Optional data removal requires explicit opt-in:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_REMOVE_DATA=true bash uninstall-dps-host.sh
```

Optional Docker volume metadata removal also requires explicit opt-in:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_REMOVE_DOCKER_VOLUMES=true bash uninstall-dps-host.sh
```

## Managed Docker Plugin

DPS can also be packaged as a Docker managed plugin:

```sh
make plugin-rootfs
docker plugin create dps:latest packaging/docker-plugin
docker plugin set dps:latest DPS_DEFAULT_VOLUME_SIZE=5G
docker plugin set dps:latest DPS_DEFAULT_VOLUME_INODES=200000
docker plugin set dps:latest DPS_ARCHIVE_POLICY=offline
docker plugin enable dps:latest
```

Compose with managed plugin name:

```yaml
volumes:
  pgdata:
    driver: dps:latest
    driver_opts:
      size: 5G
      inodes: "500000"
```

For production PaaS hosts, the unmanaged systemd service is the recommended path because it uses the host namespace directly and is easier to debug. The managed plugin package requests `CAP_SYS_ADMIN` and loop device access.

## Operations

Monitor host storage and mounts:

```sh
df -h /var/lib/dps/volume-images
du -sh /var/lib/dps/volume-images
findmnt -R /mnt/dps
losetup -a | grep /var/lib/dps || true
```

Operational notes:

- Sparse images can overcommit the host if total configured volume sizes exceed real disk capacity.
- Monitor active loop devices and mount count when running many volumes.
- Keep `DPS_IMAGE_ROOT` on fast local SSD/NVMe storage for database-heavy workloads.
- S3-compatible storage is for backup/sync, not as a live POSIX filesystem.
- DPS is local-scope storage; install it separately on each Docker host.

## Diagnostics

If a PaaS UI reports only a generic Compose error, collect host diagnostics:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/diagnose-dokploy-dps.sh -o diagnose-dokploy-dps.sh
sudo bash diagnose-dokploy-dps.sh
```

Include a small DPS create/mount/remove test:

```sh
sudo env DPS_DIAG_RUN_VOLUME_TEST=true bash diagnose-dokploy-dps.sh
```
