# Local Testing

This document covers two local test modes:

- Docker Desktop on macOS.
- A Lima Linux VM with XFS project quotas.

## Docker Desktop

Docker Desktop can load and run the DPS managed plugin:

```sh
make plugin-create
docker plugin enable dps:latest
docker volume create -d dps --name dps_lab_data -o size=64m -o inodes=1024
docker run -d --name dps-lab-vm -v dps_lab_data:/data alpine:3.22 sleep 1d
docker exec dps-lab-vm df -h /data
docker exec dps-lab-vm df -i /data
```

Docker Desktop is useful to validate plugin installation, volume mounting, `df -h`, and write-limit behavior. DPS runs in `--pool-mode auto` by default:

- It first creates or uses an XFS loopback pool under `/mnt/dps/pool`.
- If XFS project quotas are supported, it uses project quotas per volume.
- If Docker Desktop's kernel rejects `pquota/prjquota`, DPS automatically creates a fixed-size filesystem image per volume and mounts that image at the Docker volume mountpoint.

Expected result after creating a `64m` volume:

```text
Filesystem  Size  Used  Available  Use%  Mounted on
/dev/loopX  64M   ...   ...        ...   /data
```

If `df -h /data` shows the large Docker Desktop backing filesystem, the plugin is not running the current auto-pool build or it was started with `--pool-mode none/direct`.

Write-limit test:

```sh
docker exec dps-lab-vm sh -c 'dd if=/dev/zero of=/data/blob-80m bs=1M count=80 status=none'
docker exec dps-lab-vm sh -c 'du -sh /data; df -h /data; df -i /data'
```

Expected result for a `64m` volume:

```text
dd: error writing '/data/blob-80m': No space left on device
Filesystem  Size   Used   Available  Use%  Mounted on
/dev/loopX  58.5M  57.2M  0          100% /data
```

## Lima XFS Lab

Install Lima on macOS:

```sh
brew install lima
```

Create the VM:

```sh
limactl start --name dps-lab template:docker --tty=false
```

You can either let DPS create the XFS loopback pool automatically, or prepare an XFS filesystem yourself. The automatic path mirrors what the managed plugin does:

```sh
limactl shell dps-lab sh -lc '
  cd /Users/tiagobecker/Dev/Codex/docker-plugin-storage
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o /tmp/dpsd ./cmd/dpsd
  sudo rm -rf /tmp/dps-auto /tmp/dps-auto-state /tmp/dps-auto.sock
  sudo sh -c "nohup /tmp/dpsd --root /tmp/dps-auto-state --mount-root /tmp/dps-auto --pool-mode auto --pool-size 512M --socket /tmp/dps-auto.sock >/tmp/dpsd-auto.log 2>&1 &"
  sleep 2
  mount | grep /tmp/dps-auto
'
```

Manual XFS filesystem setup is still useful for production-like tests:

```sh
limactl shell dps-lab sh -lc '
  sudo apt-get update
  sudo apt-get install -y xfsprogs
  sudo mkdir -p /mnt/dps
  if ! mountpoint -q /mnt/dps; then
    sudo truncate -s 1G /var/lib/dps-lab-xfs.img
    loop=$(sudo losetup --find --show /var/lib/dps-lab-xfs.img)
    sudo mkfs.xfs -f "$loop"
    sudo mount -o pquota "$loop" /mnt/dps
  fi
  mount | grep " /mnt/dps "
  df -h /mnt/dps
'
```

Build the DPS Linux image inside the VM:

```sh
limactl shell dps-lab sh -lc '
  cd /Users/tiagobecker/Dev/Codex/docker-plugin-storage
  docker build -f packaging/Dockerfile.plugin-rootfs -t dps-plugin-rootfs:lab .
'
```

Run DPS as an unmanaged host plugin in the VM:

```sh
limactl shell dps-lab sh -lc '
  cid=$(docker create dps-plugin-rootfs:lab true)
  docker cp "$cid":/usr/local/bin/dpsd /tmp/dpsd
  docker rm "$cid"
  chmod +x /tmp/dpsd

  sudo mkdir -p /run/docker/plugins /etc/docker/plugins /mnt/dps/.state
  echo unix:///run/docker/plugins/dps.sock | sudo tee /etc/docker/plugins/dps.spec
  sudo rm -f /run/docker/plugins/dps.sock
  sudo sh -c "nohup /tmp/dpsd --root /mnt/dps/.state --mount-root /mnt/dps --socket /run/docker/plugins/dps.sock >/tmp/dpsd.log 2>&1 &"
  sleep 1
  sudo cat /tmp/dpsd.log
'
```

Use the rootful Docker daemon for quota tests:

```sh
limactl shell dps-lab sh -lc '
  sudo docker volume create -d dps --name dps_xfs_test -o size=64m -o inodes=1024
  sudo docker run -d --name dps-xfs-vm -v dps_xfs_test:/data alpine:3.22 sleep 1d
  sudo docker exec dps-xfs-vm df -h /data
  sudo docker exec dps-xfs-vm df -i /data
'
```

Validate enforcement:

```sh
limactl shell dps-lab sh -lc '
  sudo docker exec dps-xfs-vm sh -c "dd if=/dev/zero of=/data/blob-80m bs=1M count=80 status=none"
  sudo docker exec dps-xfs-vm sh -c "du -sh /data; df -h /data; df -i /data"
'
```

Expected result:

```text
dd: error writing '/data/blob-80m': No space left on device
Filesystem  Size   Used   Available  Use%  Mounted on
/dev/loop0  64.0M  64.0M  0          100% /data
```

Clean up:

```sh
limactl shell dps-lab sh -lc '
  sudo docker rm -f dps-xfs-vm
  sudo docker volume rm dps_xfs_test
  sudo pkill -f "/tmp/dpsd --root /mnt/dps/.state" || true
'
```

Stop or delete the VM:

```sh
limactl stop dps-lab
limactl delete dps-lab
```
