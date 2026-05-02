# DPS - Docker Plugin Storage

DPS is an MVP Docker volume driver focused on local, quota-aware Docker volumes with snapshot, backup, restore, and S3-compatible upload support.

This repository intentionally starts with the part Docker exposes cleanly: the `VolumeDriver` plugin API. Container writable-layer limits are handled by Docker itself through `storage_opt` and the active storage driver.

## Current Scope

- Docker `VolumeDriver` API over `/run/docker/plugins/dps.sock`.
- Named volumes created under a propagated mount root.
- Automatic XFS loopback pool when the host/plugin backing filesystem is not XFS.
- Per-volume disk and inode limits through XFS project quotas when running on Linux with `xfs_quota`.
- Automatic fixed-size per-volume filesystem fallback when the kernel does not support XFS project quotas, preserving `df -h`, `df -i`, and ENOSPC behavior.
- Plugin-native snapshots as compressed `tar.gz` archives.
- Restore into unmounted volumes.
- Backup to a local directory or `s3://bucket/prefix`.
- S3-compatible upload using AWS SigV4 with no external Go dependencies.
- Small host/admin CLI: `dpsctl`.

## Non-Goals For This MVP

- It does not replace Docker/containerd storage drivers.
- It does not control container writable layers from inside the volume plugin.
- It does not shrink mounted filesystems transparently.
- It does not mount S3 as a live POSIX filesystem.
- It does not yet provide incremental chunked backups or encrypted manifests.

For container writable layers, use Compose `storage_opt`:

```yaml
services:
  app:
    image: alpine
    command: sleep infinity
    storage_opt:
      size: 10G
```

On Docker `overlay2`, this requires XFS mounted with project quotas.

## Compose Usage

```yaml
services:
  postgres:
    image: postgres:16
    storage_opt:
      size: 12G
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
    driver: dps
    driver_opts:
      size: 50g
      inodes: "1000000"
```

For Ubuntu 24.04 arm64 servers managed by Dokploy, DPS includes a host-service installer:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

For a production-like Linux/Dokploy setup with a dedicated XFS mount, see [docs/production-dokploy.md](docs/production-dokploy.md).

## Host CLI

Build:

```sh
make build
```

Run a local plugin daemon for development:

```sh
sudo ./bin/dpsd \
  --root /var/lib/dps \
  --mount-root /mnt/dps \
  --pool-mode auto \
  --pool-size 20G \
  --socket /run/docker/plugins/dps.sock
```

Pool configuration:

```sh
--pool-mode auto        # default: use XFS mount directly, otherwise create loopback XFS
--pool-mode loop        # always create/use a loopback XFS pool
--pool-mode direct      # require mount-root itself to be XFS with project quotas
--pool-root /mnt/dps/pool
--pool-image /var/lib/dps/pool.img
--pool-size 100G
```

Default volume limit policy:

```sh
--default-volume-size 10G
--default-volume-inodes 200000
--require-limits
```

Archive consistency policy:

```sh
--archive-policy offline          # default
--archive-policy crash-consistent
--archive-policy hooked
--pre-archive-hook '...'
--post-archive-hook '...'
--archive-hook-timeout 10m
```

Policies:

| Policy | Mounted source volume | Use case |
| --- | --- | --- |
| `offline` | Refused | Default safe mode. Stop containers/writers before snapshot or `backup-volume`. |
| `crash-consistent` | Allowed | Explicit operator choice. Result is similar to capturing the volume during abrupt power loss. |
| `hooked` | Allowed after hooks | DPS runs `pre` and `post` shell hooks around snapshot or streaming backup. Use this for database-specific quiesce/checkpoint flows. |

`DPS_ALLOW_MOUNTED_ARCHIVES=true` is kept as a compatibility flag and maps to `crash-consistent` when `DPS_ARCHIVE_POLICY` is not set. Prefer `DPS_ARCHIVE_POLICY` for new setups.

Hooks receive environment variables:

```text
DPS_HOOK_PHASE=pre|post
DPS_ARCHIVE_OPERATION=snapshot|backup-volume
DPS_ARCHIVE_POLICY=hooked
DPS_ARCHIVE_ARTIFACT=<snapshot-or-backup-name>
DPS_VOLUME_NAME=<volume>
DPS_VOLUME_MOUNTPOINT=<path>
DPS_VOLUME_REFCOUNT=<docker-refcount>
```

CLI flags are global in this MVP, so pass them before the command:

```sh
dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  snapshot pgdata snap1
```

Precedence:

| Configuration | Result |
| --- | --- |
| No defaults and no `driver_opts` | Volume is created without a DPS limit. |
| Defaults set and no `driver_opts` | Defaults are applied. |
| Defaults set and `driver_opts` set | `driver_opts` win. |
| `--require-limits` without `size` or default size | Volume creation is rejected. |

With the default managed plugin config, DPS stores the sparse pool image at `/mnt/dps/.state/pool.img` and mounts it at `/mnt/dps/pool`. Docker volumes are created below `/mnt/dps/pool/volumes`.

For an arbitrary host path such as `/srv/docker-dps`, run `dpsd` as a host service/unmanaged plugin and set `--mount-root /srv/docker-dps`. Docker managed plugins need returned mountpoints to live under their configured `propagatedMount`, which is `/mnt/dps` in this package.

Runtime behavior:

- If the pool supports XFS project quotas, DPS uses one shared XFS pool and applies project quota per volume.
- If the kernel accepts XFS loop mounts but rejects `pquota/prjquota`, DPS keeps the shared pool for placement and creates a fixed-size filesystem image per volume. This is the Docker Desktop-friendly path and still makes `df -h /data` and `df -i /data` show the volume limit inside the container.
- If neither loop mounts nor filesystem creation are available, DPS fails startup or volume creation instead of silently ignoring limits.

Use the CLI:

```sh
./bin/dpsctl snapshot pgdata
./bin/dpsctl snapshots pgdata
./bin/dpsctl backup pgdata-20260501T120000Z /backup/dps
./bin/dpsctl restore pgdata-20260501T120000Z pgdata
./bin/dpsctl resize pgdata 80g 2000000
```

## Local Snapshots

DPS snapshots are local point-in-time archives intended for fast rollback on the same host. They are deliberately implemented with portable `tar.gz` files in this MVP so the feature works in native XFS mode and in compatibility image mode.

Snapshot layout:

```text
<root>/snapshots/<volume>/<snapshot>.tar.gz
<root>/snapshots/<volume>/<snapshot>.tar.gz.manifest.json
```

Safety guarantees:

- Snapshot creation writes `<snapshot>.tar.gz.tmp`, hashes the payload while writing, fsyncs, then atomically renames it into place.
- DPS writes a manifest sidecar with format, byte count, and SHA-256, then publishes the snapshot in metadata only after the archive and manifest succeed.
- Restore verifies the snapshot file against both the metadata catalog and manifest before deleting or replacing the destination volume contents.
- A corrupt, truncated, or manually modified snapshot is rejected before restore touches the volume.
- Restore into a mounted volume is refused.
- Snapshot and `backup-volume` of a mounted source volume are refused by default. Use `--archive-policy crash-consistent` for an explicit crash-consistent capture, or `--archive-policy hooked` with database/application hooks.

Performance expectations:

- In this MVP, snapshot speed is bounded by the amount of data and number of files because the archive backend must read the volume and write a compressed local file.
- It is usually fast enough for small and medium volumes, dev environments, rollback checkpoints before resize, and operational safety checkpoints.
- It is not equivalent to an instant copy-on-write filesystem snapshot. Future native backends can add XFS reflink, LVM, Btrfs, or ZFS snapshots where available, while keeping the same manifest/verification contract.

Database note: archive integrity is not the same as database transaction consistency. DPS now has the hook boundary needed for database-aware snapshots, but the hook commands are operator-supplied because correct Postgres/MySQL/MariaDB handling depends on credentials, topology, WAL/binlog policy, and whether the target is a primary, replica, or single-node database.

## Resizing Volumes

DPS supports increasing and decreasing volume limits, but the procedure depends on the active backend.

### Native XFS Project Quota Mode

This is the preferred production mode: `DPS_POOL_MODE=direct` with `--mount-root` on an XFS filesystem mounted with `prjquota`.

Resize command:

```sh
dpsctl resize pgdata 80g 2000000
```

Behavior:

- Increasing size is online from DPS's perspective: the project quota is raised.
- Decreasing size is also applied as a quota change, but DPS first checks current data usage and refuses the operation if the new target does not have at least 10% headroom.
- Increasing/decreasing inode limits works the same way through XFS project quota.
- Containers using the volume do not need a filesystem remount because the filesystem is the shared XFS pool; only the quota changes.

Recommended native workflow:

```sh
dpsctl snapshot pgdata before-resize-pgdata
dpsctl resize pgdata 80g 2000000
docker exec <container> df -h /path/in/container
docker exec <container> df -i /path/in/container
```

### Compatibility / Fixed Image Mode

This mode is used when the host or Docker Desktop cannot provide XFS project quotas. DPS creates a fixed-size filesystem image per volume and mounts it at the Docker volume mountpoint.

Resize command is the same:

```sh
dpsctl resize pgdata 80g 2000000
```

Behavior:

- Increasing size without changing inodes is handled by growing the image and filesystem.
- Decreasing size is offline: the volume must not be mounted by a container. DPS creates a temporary snapshot, unmounts the old image, recreates the image at the new size, restores the snapshot, and keeps a temporary backup image until restore succeeds.
- Changing inode count is also offline because ext-style filesystems cannot safely change inode count in place. DPS uses the same snapshot/recreate/restore routine.
- DPS refuses shrink operations if measured usage would leave less than 10% headroom in the new limit.

Recommended compatibility workflow:

```sh
docker compose stop postgres
dpsctl snapshot pgdata before-shrink-pgdata
dpsctl resize pgdata 20g 500000
docker compose up -d postgres
docker exec <container> df -h /path/in/container
docker exec <container> df -i /path/in/container
```

If the resize fails during restore, DPS attempts to roll back to the previous image. Keep an external backup for important data before shrinking.

## S3-Compatible Backup

DPS backups are manifest-based and verified. A completed backup contains:

```text
<backup-id>/
  data.tar.gz
  manifest.json
  manifest.json.sha256
```

The data object is written first, hashed with SHA-256, read back, and verified. The manifest is published last, so a backup only appears complete after the payload has been verified.

The backup writer accepts streams. `backup-volume` streams a tar.gz archive directly from the volume mountpoint into the verified backup writer, so it does not need to first create a full snapshot `.tar.gz` under DPS state. This is the preferred low-staging path for large volumes, but it follows the same mounted-volume safety policy as snapshots.

Create a backup from an existing snapshot:

```sh
dpsctl snapshot pgdata snap-pgdata
dpsctl backup snap-pgdata /backup/dps
```

Create a streaming backup directly from an unmounted volume:

```sh
dpsctl backup-volume pgdata /backup/dps
```

Create a streaming backup from a mounted volume only with an explicit consistency policy:

```sh
dpsctl --archive-policy crash-consistent backup-volume pgdata /backup/dps

dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  backup-volume pgdata s3://my-bucket/prod/postgres
```

`backup-volume` uses:

```text
volume mountpoint -> tar.gz stream -> checksum -> local/S3 writer -> read-back verify -> manifest
```

For S3-compatible targets, streaming uses multipart upload and aborts incomplete uploads on failure. DPS still reads the completed object back and checks SHA-256 before publishing `manifest.json`.

Verify and restore:

```sh
dpsctl backup-verify /backup/dps <backup-id>
dpsctl backup-restore /backup/dps <backup-id> pgdata_restored
```

For S3-compatible targets, set credentials and endpoint details:

```sh
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export DPS_S3_ENDPOINT=https://s3.amazonaws.com
export DPS_S3_REGION=us-east-1
export DPS_S3_PATH_STYLE=false
```

Then:

```sh
dpsctl backup-volume pgdata s3://my-bucket/prod/postgres
dpsctl backup-verify s3://my-bucket/prod/postgres <backup-id>
dpsctl backup-restore s3://my-bucket/prod/postgres <backup-id> pgdata_restored
```

For MinIO and many S3-compatible stores, use:

```sh
export DPS_S3_ENDPOINT=http://minio.local:9000
export DPS_S3_REGION=us-east-1
export DPS_S3_PATH_STYLE=true
```

Current safety model:

- Local backups use temporary files and atomic rename.
- S3 streaming backups use multipart upload, abort incomplete uploads on failure, read completed data back, verify SHA-256 and byte count, then publish `manifest.json`.
- Restore downloads/copies to a temporary file, verifies SHA-256 and byte count, then extracts into an unmounted volume.
- By default, `backup-volume` refuses mounted source volumes for the same reason snapshots do. `crash-consistent` and `hooked` must be selected explicitly.
- DPS refuses to restore into a mounted volume.

Application consistency is separate from backup integrity. Archive snapshots can still capture an application mid-write in `crash-consistent` mode. For databases, use `hooked` with DB-native commands or pair DPS with logical backup tooling.

## Managed Plugin Packaging

Build the plugin rootfs and create a local Docker plugin:

```sh
make plugin-rootfs
sudo docker plugin create dps:latest packaging/docker-plugin
sudo docker plugin enable dps:latest
```

Configure before enabling, or disable first and re-enable:

```sh
docker plugin disable dps:latest
docker plugin set dps:latest DPS_POOL_SIZE=100G
docker plugin set dps:latest DPS_POOL_IMAGE=/mnt/dps/.state/pools/main.img
docker plugin set dps:latest DPS_POOL_ROOT=/mnt/dps/pools/main
docker plugin set dps:latest DPS_DEFAULT_VOLUME_SIZE=10G
docker plugin set dps:latest DPS_DEFAULT_VOLUME_INODES=200000
docker plugin set dps:latest DPS_REQUIRE_LIMITS=true
docker plugin set dps:latest DPS_ARCHIVE_POLICY=offline
docker plugin set dps:latest DPS_PRE_ARCHIVE_HOOK=
docker plugin set dps:latest DPS_POST_ARCHIVE_HOOK=
docker plugin set dps:latest DPS_ARCHIVE_HOOK_TIMEOUT=10m
docker plugin enable dps:latest
```

The managed plugin requests `CAP_SYS_ADMIN` because volume plugins that mount or manage filesystem quota need privileged kernel operations.

Managed plugin state is stored under `/mnt/dps/.state`, inside Docker's propagated plugin mount. This avoids requiring a host bind mount such as `/var/lib/dps`, which is awkward on Docker Desktop and brittle across daemon environments.

For local quota validation, see [docs/local-testing.md](docs/local-testing.md). DPS creates an XFS loopback pool automatically when the backing filesystem is ext4 or another non-XFS filesystem, and falls back to fixed per-volume filesystem images when project quotas are unavailable.

## Operational Guidance

DPS has two practical operating profiles:

- **Recommended path:** a dedicated XFS disk or partition mounted with project quotas, using `DPS_POOL_MODE=direct`.
- **Compatibility path:** automatic loopback/fixed-image fallback, using the default `DPS_POOL_MODE=auto`.

The compatibility path is useful and safe for development, Docker Desktop, and constrained environments, but it has more moving parts: loop devices, extra mounts, sparse image files, and offline resize constraints. The recommended path has fewer layers and is the better target for production I/O.

Best practice by scenario:

| Scenario | Recommended setup | Notes |
| --- | --- | --- |
| Production workloads | Dedicated XFS disk/partition with `prjquota`, `DPS_POOL_MODE=direct` | Best performance and simplest quota model. |
| Docker Desktop or local development | Default `DPS_POOL_MODE=auto` | The fallback image path is adequate and makes `df -h`, `df -i`, and ENOSPC behavior visible inside containers. |
| Many small volumes | Prefer XFS project quota; fallback works with monitoring | Watch loop devices, mount count, mount latency, and total pool usage. |
| Heavy databases | Prefer direct XFS project quotas | Avoid per-volume image fallback for latency-sensitive fsync-heavy workloads when possible. |
| Multi-tenant hosts | Limit both per-volume quota and total pool size | Sparse images can overcommit the host if the pool/device is not monitored. |

Performance expectations:

- Direct XFS project quotas are the fastest DPS mode.
- Loopback/fixed-image volumes add an extra filesystem and loop layer. Sequential I/O is usually acceptable; random small writes and fsync-heavy workloads can feel the overhead.
- S3-compatible storage should be treated as backup/sync storage, not as a live POSIX filesystem for databases.

Operational limits to monitor:

- Number of active loop devices.
- Number of mounted volumes.
- Real disk usage of sparse pool and volume images.
- Inode usage per volume via `df -i`.
- Mount/unmount errors after host crashes or forced shutdowns.

## Production Hardening Roadmap

- Replace archive snapshots with filesystem-native snapshots for Btrfs/ZFS and XFS reflink-assisted copies where available.
- Add incremental backup manifests, chunking, compression strategy selection, and client-side encryption.
- Add fs-freeze and pre/post snapshot hooks for database consistency.
- Add a small Compose wrapper for validating `x-dps` policy blocks and generating `driver_opts`.
- Add containerd snapshotter support for first-class container writable-layer management.
- Add Prometheus metrics and structured audit logs.
- Add destructive integration tests in a Linux VM with XFS `prjquota`.
