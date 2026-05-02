# Local Testing

DPS uses one local backend: each Docker volume is an ext4 image file mounted through a loop device. Local tests should therefore validate:

- plugin installation;
- volume creation with `driver: dps`;
- `df -h` size limit;
- `df -i` inode limit;
- write failure at the configured limit.

DPS returns the `data` subdirectory inside the mounted image to Docker, not the
filesystem root. This keeps ext metadata such as `lost+found` away from
database images that require an empty data directory.

## Docker Desktop

```sh
make plugin-rootfs
docker plugin create dps-test:local packaging/docker-plugin
docker plugin enable dps-test:local
docker volume create -d dps-test:local --name dps_lab_data -o size=64m -o inodes=1024
docker run -d --name dps-lab-vm -v dps_lab_data:/data alpine:3.22 sleep 1d
docker exec dps-lab-vm df -h /data
docker exec dps-lab-vm df -i /data
```

Expected shape:

```text
Filesystem  Size  Used  Available  Use%  Mounted on
/dev/loopX  64M   ...   ...        ...   /data
```

Write-limit test:

```sh
docker exec dps-lab-vm sh -c 'dd if=/dev/zero of=/data/blob-80m bs=1M count=80 status=none'
docker exec dps-lab-vm sh -c 'du -sh /data; df -h /data; df -i /data'
```

Expected result for a `64m` volume:

```text
dd: error writing '/data/blob-80m': No space left on device
```

Clean up:

```sh
docker rm -f dps-lab-vm
docker volume rm dps_lab_data
docker plugin rm -f dps-test:local
```

## Linux VM

Any rootful Linux VM with Docker is enough. The host root filesystem may be ext4.

Build DPS:

```sh
make build
```

Copy `bin/dpsd` and `bin/dpsctl` into the VM, then run:

```sh
sudo apt-get update
sudo apt-get install -y e2fsprogs util-linux

sudo install -m 0755 dpsd /usr/local/bin/dpsd
sudo mkdir -p /run/docker/plugins /var/lib/dps /mnt/dps
sudo rm -f /run/docker/plugins/dps.sock

sudo sh -c 'nohup /usr/local/bin/dpsd \
  --root /var/lib/dps \
  --mount-root /mnt/dps \
  --default-volume-size 5G \
  --default-volume-inodes 200000 \
  --socket /run/docker/plugins/dps.sock \
  >/tmp/dpsd.log 2>&1 &'
```

Create and validate a volume:

```sh
sudo docker volume create -d dps --name dps_test -o size=64m -o inodes=1024
sudo docker run -d --name dps-test-vm -v dps_test:/data alpine:3.22 sleep 1d
sudo docker exec dps-test-vm df -h /data
sudo docker exec dps-test-vm df -i /data
sudo docker exec dps-test-vm sh -c 'dd if=/dev/zero of=/data/blob-80m bs=1M count=80 status=none'
```

Inspect host state:

```sh
sudo findmnt -R /mnt/dps
sudo losetup -a | grep dps || true
sudo du -sh /var/lib/dps/volume-images
```

Clean up:

```sh
sudo docker rm -f dps-test-vm
sudo docker volume rm dps_test
sudo pkill -f '/usr/local/bin/dpsd' || true
```
