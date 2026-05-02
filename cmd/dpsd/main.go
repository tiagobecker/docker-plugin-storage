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
	poolMode := flag.String("pool-mode", getenv("DPS_POOL_MODE", "auto"), "pool mode: auto, loop, direct, none")
	poolRoot := flag.String("pool-root", getenv("DPS_POOL_ROOT", ""), "XFS pool mountpoint; defaults to <mount-root>/pool")
	poolImage := flag.String("pool-image", getenv("DPS_POOL_IMAGE", ""), "loopback XFS image path; defaults to <root>/pool.img")
	poolSize := flag.String("pool-size", getenv("DPS_POOL_SIZE", "20G"), "loopback XFS pool size")
	defaultVolumeSize := flag.String("default-volume-size", getenv("DPS_DEFAULT_VOLUME_SIZE", ""), "default volume size when driver_opts.size is omitted")
	defaultVolumeInodes := flag.String("default-volume-inodes", getenv("DPS_DEFAULT_VOLUME_INODES", ""), "default inode limit when driver_opts.inodes is omitted")
	requireLimits := flag.Bool("require-limits", getenvBool("DPS_REQUIRE_LIMITS", false), "reject volumes without size limit or default size")
	allowMountedArchives := flag.Bool("allow-mounted-archives", getenvBool("DPS_ALLOW_MOUNTED_ARCHIVES", false), "allow snapshot and backup-volume while a volume is mounted")
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
		Root:                 *root,
		MountRoot:            *mountRoot,
		PoolMode:             *poolMode,
		PoolRoot:             *poolRoot,
		PoolImage:            *poolImage,
		PoolSize:             *poolSize,
		DefaultSize:          *defaultVolumeSize,
		DefaultInodes:        *defaultVolumeInodes,
		RequireLimits:        *requireLimits,
		AllowMountedArchives: *allowMountedArchives,
		ArchivePolicy:        *archivePolicy,
		PreArchiveHook:       *preArchiveHook,
		PostArchiveHook:      *postArchiveHook,
		ArchiveHookTimeout:   hookTimeout,
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

func getenvBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}
