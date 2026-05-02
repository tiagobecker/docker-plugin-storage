# Dokploy Setup

This guide describes the DPS path for real Dokploy-managed Linux servers.

DPS now has one storage model: every Docker volume is backed by its own ext4 filesystem image and mounted through a loop device. This is the expected path on ordinary Ubuntu cloud images, including hosts where the root filesystem is ext4.

```text
Dokploy Compose
  -> Docker creates volume with driver: dps
  -> DPS creates /var/lib/dps/volume-images/<volume>.img
  -> DPS formats it as ext4 with the requested inode count
  -> DPS mounts it at /mnt/dps/volumes/<volume>
  -> Docker bind-mounts that path into the container
```

Dokploy does not need to know about the backing image. It only needs Compose volumes using `driver: dps`.

## Upgrade Note

If this host used an earlier DPS build with multiple storage backends, treat this version as a storage-layout change. For disposable test hosts, remove old DPS volumes and state before redeploying. For data that matters, back up first, create fresh DPS volumes with this version, and restore into them.

## Install On Ubuntu 24.04 arm64

Run this on each Docker host managed by Dokploy:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

Defaults:

- `DPS_ROOT=/var/lib/dps`
- `DPS_IMAGE_ROOT=/var/lib/dps/volume-images`
- `DPS_MOUNT_ROOT=/mnt/dps`
- `DPS_DEFAULT_VOLUME_SIZE=10G`
- `DPS_DEFAULT_VOLUME_INODES=200000`
- `DPS_ARCHIVE_POLICY=offline`

The installer is intentionally conservative:

- it refuses to run if a managed Docker plugin named like `dps` already exists;
- it removes stale unmanaged DPS `.spec` files;
- it removes a stale DPS socket before starting `dpsd`;
- it creates and mounts a small DPS test volume before reporting success;
- it stops/removes the `dpsd` systemd unit and socket if that test fails.

If the conflict check is intentionally too strict:

```sh
sudo env DPS_INSTALL_ALLOW_MANAGED_PLUGIN_CONFLICT=true bash install-dps.sh
```

To place volume data on a different mounted disk or directory, set `DPS_IMAGE_ROOT`:

```sh
sudo env DPS_IMAGE_ROOT=/srv/dps-images bash install-dps.sh
```

Use a location backed by enough local storage. DPS does not format disks or create partitions.

## Validate The Service

```sh
systemctl status dpsd
journalctl -u dpsd -n 100 --no-pager
cat /etc/dps/dpsd.env
```

Expected config shape:

```text
DPS_ROOT=/var/lib/dps
DPS_MOUNT_ROOT=/mnt/dps
DPS_IMAGE_ROOT=/var/lib/dps/volume-images
```

## Use DPS In Dokploy Compose

Example:

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
      size: 20G
      inodes: "500000"
```

If the global defaults are acceptable, the volume may omit `driver_opts`:

```yaml
volumes:
  appdata:
    driver: dps
```

## Validate Limits

Dokploy or Compose may prefix the real volume name with the project/app name.

```sh
docker volume ls | grep pgdata
docker volume inspect <real-volume-name>
```

Check the mounted path inside the container:

```sh
docker ps
docker exec -it <container> df -h /var/lib/postgresql/data
docker exec -it <container> df -i /var/lib/postgresql/data
```

Expected shape:

```text
Filesystem      Size  Used Avail Use% Mounted on
/dev/loopX       20G  ...   ...   ... /var/lib/postgresql/data

Filesystem     Inodes IUsed IFree IUse% Mounted on
/dev/loopX      500000 ...   ...   ... /var/lib/postgresql/data
```

`/dev/loopX` is correct. It means DPS mounted the per-volume filesystem image.

## Reset A Test Host

Only use this on a disposable test host where DPS volumes can be lost.

Stop Dokploy apps that use DPS volumes, remove old test volumes from Docker when possible, then run:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/reset-dps-host.sh -o reset-dps-host.sh
sudo bash reset-dps-host.sh
```

For non-interactive use:

```sh
sudo env DPS_RESET_CONFIRM=erase-dps bash reset-dps-host.sh
```

The reset script removes:

- `dpsd` systemd service;
- `/etc/dps`;
- `/usr/local/bin/dpsd` and `/usr/local/bin/dpsctl`;
- `/run/docker/plugins/dps.sock`;
- `/etc/docker/plugins/dps.spec`;
- `/var/lib/dps`;
- `/mnt/dps`;
- `/opt/docker-plugin-storage`.

Reinstall:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

Docker volume metadata may still list old DPS volumes. Remove them before redeploying:

```sh
docker volume ls
docker volume rm <old-volume>
```

## Deep Uninstall

If a test host needs to return to a clean state before reinstalling DPS, use the
deep uninstall script. It removes only DPS-related state:

- Docker volumes using `driver: dps` or `driver: dps:*`;
- managed Docker plugins named like `dps` or `docker-plugin-storage`;
- unmanaged plugin socket/spec files;
- `dpsd`/`dps` systemd services;
- DPS loop mounts and loop devices;
- `/etc/dps`, `/var/lib/dps`, `/mnt/dps`, `/opt/docker-plugin-storage`;
- `/usr/local/bin/dpsd` and `/usr/local/bin/dpsctl`.

It does not remove Docker, Dokploy, ordinary `local` volumes, or unrelated
containers.

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/uninstall-dps-host.sh -o uninstall-dps-host.sh
sudo bash uninstall-dps-host.sh
```

For non-interactive use:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps bash uninstall-dps-host.sh
```

On a disposable test host where Docker should be restarted after cleanup:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_RESTART_DOCKER=true bash uninstall-dps-host.sh
```

If normal unmount fails because the host has stale busy mounts and all DPS data
can be discarded:

```sh
sudo env DPS_UNINSTALL_CONFIRM=erase-dps DPS_UNINSTALL_LAZY_UNMOUNT=true DPS_UNINSTALL_RESTART_DOCKER=true bash uninstall-dps-host.sh
```

## Diagnose Deploy Failures

When Dokploy only reports `Error starting compose`, first separate the failure:

- Docker daemon unhealthy;
- Dokploy/Swarm failing before the volume is created;
- existing Docker volume still using the `local` driver;
- DPS service/socket/mount failure.

Run the diagnostic script on the Docker host:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/diagnose-dokploy-dps.sh -o diagnose-dokploy-dps.sh
sudo bash diagnose-dokploy-dps.sh
```

Optional DPS driver test:

```sh
sudo env DPS_DIAG_RUN_VOLUME_TEST=true bash diagnose-dokploy-dps.sh
```

If a previously deployed app created the volume without DPS, `docker volume inspect <real-volume-name>` will show `"Driver": "local"`. Docker will not convert that volume to `dps`; remove the stopped app/service and the old volume, then redeploy.

## Resize

Resize is offline. Stop the app in Dokploy first.

```sh
dpsctl resize <volume> 40G 800000
```

Increasing size grows the ext4 image when possible. Shrinking size or changing inode count recreates the image and restores data after checking that current usage fits with 10% headroom.

## Snapshots And Backups

Default policy is `offline`, so snapshot and `backup-volume` are refused while the volume is mounted by Docker.

```sh
dpsctl snapshot pgdata before-upgrade
dpsctl backup before-upgrade /srv/dps-backups
dpsctl backup-volume pgdata s3://bucket/prod pgdata-manual
```

For databases, stop writes or use tested hooks:

```sh
dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  backup-volume pgdata s3://bucket/prod pgdata-hooked
```

## Managed Plugin Alternative

```sh
make plugin-rootfs
sudo docker plugin create dps:latest packaging/docker-plugin
sudo docker plugin set dps:latest DPS_DEFAULT_VOLUME_SIZE=10G
sudo docker plugin set dps:latest DPS_DEFAULT_VOLUME_INODES=200000
sudo docker plugin set dps:latest DPS_ARCHIVE_POLICY=offline
sudo docker plugin enable dps:latest
```

The managed plugin package requests `CAP_SYS_ADMIN` and loop device access. It
declares `/dev/loop-control` and `/dev/loop0` through `/dev/loop7`, which is
needed for Docker Desktop managed plugin tests. For production Dokploy hosts,
the unmanaged systemd service remains the recommended path because it uses the
host device namespace directly.

Compose can then use:

```yaml
volumes:
  pgdata:
    driver: dps:latest
    driver_opts:
      size: 20G
      inodes: "500000"
```
