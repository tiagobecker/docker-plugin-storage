package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/devpower/dps/internal/driver"
)

func main() {
	root := flag.String("root", getenv("DPS_ROOT", "/var/lib/dps"), "state root")
	mountRoot := flag.String("mount-root", getenv("DPS_MOUNT_ROOT", "/mnt/dps"), "propagated mount root")
	imageRoot := flag.String("image-root", getenv("DPS_IMAGE_ROOT", ""), "volume image root; defaults to <root>/volume-images")
	defaultVolumeSize := flag.String("default-volume-size", getenv("DPS_DEFAULT_VOLUME_SIZE", "5G"), "default volume size when driver_opts.size is omitted")
	defaultVolumeInodes := flag.String("default-volume-inodes", getenv("DPS_DEFAULT_VOLUME_INODES", "200000"), "default inode limit when driver_opts.inodes is omitted")
	archivePolicy := flag.String("archive-policy", getenv("DPS_ARCHIVE_POLICY", ""), "archive policy: offline, crash-consistent, hooked")
	preArchiveHook := flag.String("pre-archive-hook", getenv("DPS_PRE_ARCHIVE_HOOK", ""), "shell command to run before snapshot or backup-volume in hooked policy")
	postArchiveHook := flag.String("post-archive-hook", getenv("DPS_POST_ARCHIVE_HOOK", ""), "shell command to run after snapshot or backup-volume in hooked policy")
	archiveHookTimeout := flag.String("archive-hook-timeout", getenv("DPS_ARCHIVE_HOOK_TIMEOUT", "10m"), "hook timeout as Go duration, for example 30s or 10m")
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	hookTimeout, err := time.ParseDuration(*archiveHookTimeout)
	if err != nil {
		log.Fatalf("invalid archive hook timeout: %v", err)
	}
	d, err := driver.NewWithOptions(driver.Options{
		Root:               *root,
		MountRoot:          *mountRoot,
		ImageRoot:          *imageRoot,
		DefaultSize:        *defaultVolumeSize,
		DefaultInodes:      *defaultVolumeInodes,
		ArchivePolicy:      *archivePolicy,
		PreArchiveHook:     *preArchiveHook,
		PostArchiveHook:    *postArchiveHook,
		ArchiveHookTimeout: hookTimeout,
	})
	if err != nil {
		log.Fatal(err)
	}

	switch flag.Arg(0) {
	case "create":
		if flag.NArg() < 2 {
			log.Fatal("usage: dpsctl create <volume> [size] [inodes]")
		}
		opts := map[string]string{}
		if flag.NArg() > 2 {
			opts["size"] = flag.Arg(2)
		}
		if flag.NArg() > 3 {
			opts["inodes"] = flag.Arg(3)
		}
		v, err := d.Create(flag.Arg(1), opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\t%s\tsize=%s\tinodes=%s\n", v.Name, v.Mountpoint, v.Size, v.Inodes)
	case "list":
		for _, v := range d.Store.ListVolumes() {
			fmt.Printf("%s\t%s\tsize=%s\tinodes=%s\trefs=%d\n", v.Name, v.Mountpoint, v.Size, v.Inodes, v.RefCount)
		}
	case "snapshots":
		volume := ""
		if flag.NArg() > 1 {
			volume = flag.Arg(1)
		}
		for _, sn := range d.Store.ListSnapshots(volume) {
			fmt.Printf("%s\tvolume=%s\tbytes=%d\tpath=%s\n", sn.Name, sn.Volume, sn.Bytes, sn.Path)
		}
	case "snapshot":
		if flag.NArg() < 2 {
			log.Fatal("usage: dpsctl snapshot <volume> [name]")
		}
		name := ""
		if flag.NArg() > 2 {
			name = flag.Arg(2)
		}
		sn, err := d.Snapshot(flag.Arg(1), name)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(sn.Name)
	case "backup":
		if flag.NArg() != 3 {
			log.Fatal("usage: dpsctl backup <snapshot> <target-dir|s3://bucket/prefix>")
		}
		manifest, err := d.Backup(flag.Arg(1), flag.Arg(2))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\tbytes=%d\tsha256=%s\n", manifest.BackupID, manifest.DataBytes, manifest.DataSHA256)
	case "backup-volume":
		if flag.NArg() < 3 {
			log.Fatal("usage: dpsctl backup-volume <volume> <target-dir|s3://bucket/prefix> [snapshot-name]")
		}
		snapshotName := ""
		if flag.NArg() > 3 {
			snapshotName = flag.Arg(3)
		}
		manifest, err := d.BackupVolume(flag.Arg(1), flag.Arg(2), snapshotName)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\tbytes=%d\tsha256=%s\n", manifest.BackupID, manifest.DataBytes, manifest.DataSHA256)
	case "backup-verify":
		if flag.NArg() != 3 {
			log.Fatal("usage: dpsctl backup-verify <target-dir|s3://bucket/prefix> <backup-id>")
		}
		manifest, err := d.VerifyBackup(flag.Arg(1), flag.Arg(2))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\tverified\tbytes=%d\tsha256=%s\n", manifest.BackupID, manifest.DataBytes, manifest.DataSHA256)
	case "backup-restore":
		if flag.NArg() < 3 {
			log.Fatal("usage: dpsctl backup-restore <target-dir|s3://bucket/prefix> <backup-id> [volume]")
		}
		volume := ""
		if flag.NArg() > 3 {
			volume = flag.Arg(3)
		}
		if err := d.RestoreBackup(flag.Arg(1), flag.Arg(2), volume); err != nil {
			log.Fatal(err)
		}
	case "restore":
		if flag.NArg() < 2 {
			log.Fatal("usage: dpsctl restore <snapshot> [volume]")
		}
		volume := ""
		if flag.NArg() > 2 {
			volume = flag.Arg(2)
		}
		if err := d.Restore(flag.Arg(1), volume); err != nil {
			log.Fatal(err)
		}
	case "resize":
		if flag.NArg() < 3 {
			log.Fatal("usage: dpsctl resize <volume> <size> [inodes]")
		}
		inodes := ""
		if flag.NArg() > 3 {
			inodes = flag.Arg(3)
		}
		if err := d.Resize(flag.Arg(1), flag.Arg(2), inodes); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `dpsctl manages DPS volumes.

Global archive flags must be placed before the command:
  --archive-policy offline|crash-consistent|hooked
  --pre-archive-hook <shell-command>
  --post-archive-hook <shell-command>
  --archive-hook-timeout 10m

Commands:
  create <volume> [size] [inodes]
  list
  snapshots [volume]
  snapshot <volume> [name]
  backup <snapshot> <target-dir|s3://bucket/prefix>
  backup-volume <volume> <target-dir|s3://bucket/prefix> [snapshot-name]
  backup-verify <target-dir|s3://bucket/prefix> <backup-id>
  backup-restore <target-dir|s3://bucket/prefix> <backup-id> [volume]
  restore <snapshot> [volume]
  resize <volume> <size> [inodes]
`)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
