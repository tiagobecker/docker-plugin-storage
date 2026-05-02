# Production-Like Dokploy Setup

This guide describes the recommended way to test DPS on a real Linux cloud server running Docker and Dokploy.

The storage decision is made on the Docker host, not inside application images. Most cloud Ubuntu images use an ext4 root disk. In that common case DPS should run in `auto` mode and create its own XFS loopback pool. That is the universal path.

The preferred high-I/O production setup, when the host has storage prepared for it, is:

```text
Linux host
  dedicated XFS disk or partition mounted at /mnt/dps with prjquota
  dpsd running as a host service
  Docker using the dps volume driver socket
  Dokploy deploying Compose files that declare driver: dps
```

Dokploy does not need to know about XFS. Dokploy sends Compose definitions to Docker; Docker calls the DPS volume driver; DPS stores and limits the volumes under the selected DPS backing store.

## 1. Quick Installer For Ubuntu 24.04 arm64

For Ubuntu 24.04 on Ampere/AArch64 with Docker already installed by Dokploy, use the installer below on each Docker host where Dokploy will deploy apps:

```sh
curl -fsSL https://raw.githubusercontent.com/tiagobecker/docker-plugin-storage/main/scripts/install-ubuntu-24.04-arm64-dokploy.sh -o install-dps.sh
sudo bash install-dps.sh
```

Default behavior:

- Installs `dpsd` and `dpsctl` into `/usr/local/bin`.
- Runs DPS as a host systemd service named `dpsd`.
- Uses `DPS_POOL_MODE=auto`, so ordinary ext4 hosts run through the loopback XFS compatibility path.
- Sets default volume limits to `10G` and `200000` inodes.
- Requires limits by default with `DPS_REQUIRE_LIMITS=true`.

This is expected on most single-disk cloud images:

```sh
/dev/loopX xfs ... /mnt/dps/pool
```

It means DPS is using an XFS filesystem image on top of the host filesystem. Containers still see the configured `df -h`, `df -i`, and `ENOSPC` limits.

For direct XFS mode, prepare `/mnt/dps` manually using a real block device, LVM logical volume, or existing XFS partition, then run:

```sh
sudo env \
  DPS_POOL_MODE=direct \
  DPS_MOUNT_ROOT=/mnt/dps \
  bash install-dps.sh
```

For a different mount location:

```sh
sudo env \
  DPS_POOL_MODE=direct \
  DPS_MOUNT_ROOT=/srv/dps \
  bash install-dps.sh
```

The installer refuses `DPS_POOL_MODE=direct` unless the mountpoint is already XFS with `prjquota` or `pquota`. It does not partition or format disks.

Run the script on every Dokploy-managed Docker server that should support `driver: dps`.

## 2. Prepare XFS

This manual path is useful only when the server has a dedicated block device, LVM logical volume, or spare partition. The example below uses `/dev/vdb`.

Warning: `mkfs.xfs` erases the target device.

```sh
lsblk -f

sudo apt-get update
sudo apt-get install -y xfsprogs

sudo mkfs.xfs -f /dev/vdb
sudo mkdir -p /mnt/dps

UUID=$(sudo blkid -s UUID -o value /dev/vdb)
echo "UUID=$UUID /mnt/dps xfs defaults,prjquota 0 2" | sudo tee -a /etc/fstab

sudo mount -a
findmnt -no SOURCE,FSTYPE,OPTIONS /mnt/dps
```

The output must show `xfs` and `prjquota` or `pquota`.

## 3. Manual Host Plugin Install

The quick installer above is the recommended path for Ubuntu 24.04 arm64. For manual installs, build on the server:

```sh
git clone https://github.com/<owner>/docker-plugin-storage.git /opt/dps
cd /opt/dps

make build

sudo install -m 0755 bin/dpsd /usr/local/bin/dpsd
sudo install -m 0755 bin/dpsctl /usr/local/bin/dpsctl
```

Create the systemd service:

```sh
sudo tee /etc/systemd/system/dpsd.service >/dev/null <<'EOF'
[Unit]
Description=DPS Docker Volume Driver
After=docker.service
Requires=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/dpsd \
  --root /var/lib/dps \
  --mount-root /mnt/dps \
  --pool-mode direct \
  --default-volume-size 10G \
  --default-volume-inodes 200000 \
  --require-limits \
  --archive-policy offline \
  --socket /run/docker/plugins/dps.sock
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

sudo mkdir -p /var/lib/dps /run/docker/plugins
sudo systemctl daemon-reload
sudo systemctl enable --now dpsd
sudo systemctl status dpsd
```

## 4. Validate Docker Volume Limits

Create a small test volume:

```sh
docker volume create \
  --driver dps \
  --opt size=1G \
  --opt inodes=50000 \
  dps_test
```

Check visible limits from inside a container:

```sh
docker run --rm -v dps_test:/data alpine:3.22 df -h /data
docker run --rm -v dps_test:/data alpine:3.22 df -i /data
```

Write past the limit:

```sh
docker run --rm -v dps_test:/data alpine:3.22 \
  sh -c 'dd if=/dev/zero of=/data/blob bs=1M count=1200 status=progress'
```

Expected result: the write fails near the configured limit with `No space left on device`.

## 5. Use DPS In Dokploy Compose

Dokploy templates must declare DPS as the volume driver. A plain Compose volume such as `pgdata:` uses Docker's default local driver, not DPS.

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

If DPS runs with defaults and `--require-limits`, the template can omit `driver_opts` only when the global defaults are acceptable:

```yaml
volumes:
  pgdata:
    driver: dps
```

After deployment, validate the actual container path:

```sh
docker ps
docker exec -it <container> df -h /var/lib/postgresql/data
docker exec -it <container> df -i /var/lib/postgresql/data
```

Dokploy or Compose may prefix the real volume name with the project/app name. Inspect it with:

```sh
docker volume ls | grep pgdata
docker volume inspect <real-volume-name>
```

## 6. Managed Plugin Alternative

The managed Docker plugin path is useful for local tests and environments where you want Docker to own the plugin lifecycle.

```sh
cd /opt/dps
make plugin-rootfs

sudo docker plugin create dps:latest packaging/docker-plugin
sudo docker plugin set dps:latest DPS_POOL_MODE=auto
sudo docker plugin set dps:latest DPS_POOL_SIZE=100G
sudo docker plugin set dps:latest DPS_DEFAULT_VOLUME_SIZE=10G
sudo docker plugin set dps:latest DPS_DEFAULT_VOLUME_INODES=200000
sudo docker plugin set dps:latest DPS_REQUIRE_LIMITS=true
sudo docker plugin set dps:latest DPS_ARCHIVE_POLICY=offline
sudo docker plugin enable dps:latest
```

Compose can then use:

```yaml
volumes:
  pgdata:
    driver: dps:latest
    driver_opts:
      size: 20G
      inodes: "500000"
```

For production-like XFS tests, prefer the host service mode above. It points directly at the host XFS mount and avoids the additional constraints of Docker managed plugin propagation.

## 7. Snapshot And Backup Policy

For databases, start with `--archive-policy offline`: stop the app in Dokploy, snapshot or backup, then start it again.

Mounted-volume capture requires an explicit policy:

```sh
dpsctl --archive-policy crash-consistent snapshot pgdata snap1

dpsctl --archive-policy hooked \
  --pre-archive-hook '/etc/dps/hooks/postgres-pre.sh' \
  --post-archive-hook '/etc/dps/hooks/postgres-post.sh' \
  backup-volume pgdata s3://bucket/prod/postgres
```

Use `hooked` only after the hook scripts have been tested against the target database.
