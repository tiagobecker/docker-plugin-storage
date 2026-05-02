package main

import (
	"flag"
	"log"
	"os"
	"time"

	"github.com/devpower/dps/internal/driver"
	"github.com/devpower/dps/internal/plugin"
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
	socket := flag.String("socket", getenv("DPS_SOCKET", "/run/docker/plugins/dps.sock"), "Docker plugin Unix socket")
	flag.Parse()

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
	if err := plugin.New(d).Listen(*socket); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
