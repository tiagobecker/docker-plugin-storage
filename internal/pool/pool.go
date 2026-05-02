package pool

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	ModeAuto   = "auto"
	ModeLoop   = "loop"
	ModeDirect = "direct"
	ModeNone   = "none"
)

type Config struct {
	Mode      string
	Root      string
	MountRoot string
	PoolRoot  string
	ImagePath string
	ImageSize string
}

type Pool struct {
	Root                 string
	QuotaRoot            string
	Mode                 string
	SupportsProjectQuota bool
}

func Ensure(cfg Config) (*Pool, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeAuto
	}
	if cfg.MountRoot == "" {
		return nil, errors.New("mount root is required")
	}
	if cfg.Root == "" {
		cfg.Root = filepath.Join(cfg.MountRoot, ".state")
	}
	if cfg.PoolRoot == "" {
		cfg.PoolRoot = filepath.Join(cfg.MountRoot, "pool")
	}
	if cfg.ImagePath == "" {
		cfg.ImagePath = filepath.Join(cfg.Root, "pool.img")
	}
	if cfg.ImageSize == "" {
		cfg.ImageSize = "20G"
	}

	if runtime.GOOS != "linux" || cfg.Mode == ModeNone {
		return &Pool{Root: cfg.MountRoot, QuotaRoot: cfg.MountRoot, Mode: ModeNone}, nil
	}
	if err := os.MkdirAll(cfg.MountRoot, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, err
	}

	switch cfg.Mode {
	case ModeDirect:
		if err := requireXFSProjectQuota(cfg.MountRoot); err != nil {
			return nil, err
		}
		return &Pool{Root: cfg.MountRoot, QuotaRoot: cfg.MountRoot, Mode: ModeDirect, SupportsProjectQuota: true}, nil
	case ModeLoop:
		return ensureLoop(cfg)
	case ModeAuto:
		if isXFSProjectQuota(cfg.MountRoot) {
			return &Pool{Root: cfg.MountRoot, QuotaRoot: cfg.MountRoot, Mode: ModeDirect, SupportsProjectQuota: true}, nil
		}
		return ensureLoop(cfg)
	default:
		return nil, fmt.Errorf("invalid pool mode %q", cfg.Mode)
	}
}

func ensureLoop(cfg Config) (*Pool, error) {
	for _, binary := range []string{"mkfs.xfs", "mount"} {
		if _, err := findBinary(binary); err != nil {
			return nil, fmt.Errorf("pool mode %q requires %s: %w", cfg.Mode, binary, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ImagePath), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.PoolRoot, 0o755); err != nil {
		return nil, err
	}
	if isMountPoint(cfg.PoolRoot) {
		return &Pool{Root: cfg.PoolRoot, QuotaRoot: cfg.PoolRoot, Mode: ModeLoop, SupportsProjectQuota: isXFSProjectQuota(cfg.PoolRoot)}, nil
	}

	created, err := ensureSparseFile(cfg.ImagePath, cfg.ImageSize)
	if err != nil {
		return nil, err
	}
	if created {
		if err := run("mkfs.xfs", "-f", cfg.ImagePath); err != nil {
			return nil, err
		}
	}
	supportsProjectQuota, err := mountXFSLoop(cfg.ImagePath, cfg.PoolRoot)
	if err != nil {
		return nil, err
	}
	return &Pool{Root: cfg.PoolRoot, QuotaRoot: cfg.PoolRoot, Mode: ModeLoop, SupportsProjectQuota: supportsProjectQuota}, nil
}

func ensureSparseFile(path, size string) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		bytes, err := ParseSize(size)
		if err != nil {
			return false, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return false, err
		}
		defer f.Close()
		if err := f.Truncate(bytes); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if st.Size() == 0 {
		bytes, err := ParseSize(size)
		if err != nil {
			return false, err
		}
		if err := os.Truncate(path, bytes); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func mountXFSLoop(image, target string) (bool, error) {
	err := run("mount", "-o", "loop,pquota", image, target)
	if err == nil {
		return true, nil
	}
	if err2 := run("mount", "-o", "loop,prjquota", image, target); err2 == nil {
		return true, nil
	}
	if err3 := run("mount", "-o", "loop", image, target); err3 != nil {
		return false, err
	}
	return false, nil
}

func requireXFSProjectQuota(path string) error {
	if !isXFSProjectQuota(path) {
		return fmt.Errorf("%s is not mounted as XFS with project quotas", path)
	}
	return nil
}

func isXFSProjectQuota(path string) bool {
	info, err := mountInfoFor(path)
	if err != nil {
		return false
	}
	if info.fsType != "xfs" {
		return false
	}
	return hasOption(info.superOptions, "pquota") || hasOption(info.superOptions, "prjquota")
}

func isMountPoint(path string) bool {
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		target = filepath.Clean(path)
	}
	info, err := mountInfoFor(path)
	return err == nil && info.mountPoint == target
}

type mountInfo struct {
	mountPoint   string
	fsType       string
	superOptions []string
}

func mountInfoFor(path string) (mountInfo, error) {
	target, err := filepath.Abs(path)
	if err != nil {
		return mountInfo{}, err
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return mountInfo{}, err
		}
		target = filepath.Clean(path)
	}

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return mountInfo{}, err
	}
	defer f.Close()

	var best mountInfo
	bestLen := -1
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		info, ok := parseMountInfo(scanner.Text())
		if !ok {
			continue
		}
		mp := info.mountPoint
		if target == mp || (mp == "/" && strings.HasPrefix(target, "/")) || strings.HasPrefix(target, mp+"/") {
			if len(mp) > bestLen {
				best = info
				bestLen = len(mp)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return mountInfo{}, err
	}
	if bestLen < 0 {
		return mountInfo{}, os.ErrNotExist
	}
	return best, nil
}

func parseMountInfo(line string) (mountInfo, bool) {
	parts := strings.Split(line, " - ")
	if len(parts) != 2 {
		return mountInfo{}, false
	}
	left := strings.Fields(parts[0])
	right := strings.Fields(parts[1])
	if len(left) < 5 || len(right) < 3 {
		return mountInfo{}, false
	}
	mp, err := strconv.Unquote(`"` + strings.ReplaceAll(left[4], `"`, `\"`) + `"`)
	if err != nil {
		mp = left[4]
	}
	mp = decodeMountPath(mp)
	return mountInfo{
		mountPoint:   mp,
		fsType:       right[0],
		superOptions: strings.Split(right[2], ","),
	}, true
}

func decodeMountPath(path string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(path)
}

func hasOption(options []string, want string) bool {
	for _, opt := range options {
		if opt == want {
			return true
		}
	}
	return false
}

func run(name string, args ...string) error {
	binary, err := findBinary(name)
	if err != nil {
		return err
	}
	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func EnsureVolumeImage(imagePath, mountPoint, size, inodes string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if isMountPoint(mountPoint) {
		return nil
	}
	if size == "" {
		return errors.New("volume image size is required")
	}
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	created, err := ensureSparseFile(imagePath, size)
	if err != nil {
		return err
	}
	if created {
		if _, err := findBinary("mkfs.ext4"); err == nil {
			args := []string{"-F", "-q"}
			if inodes != "" {
				args = append(args, "-N", inodes)
			}
			args = append(args, imagePath)
			if err := run("mkfs.ext4", args...); err != nil {
				return err
			}
		} else {
			if err := run("mkfs.xfs", "-f", imagePath); err != nil {
				return err
			}
		}
	}
	return run("mount", "-o", "loop", imagePath, mountPoint)
}

func GrowVolumeImage(imagePath, mountPoint, size string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if size == "" {
		return errors.New("volume image size is required")
	}
	bytes, err := ParseSize(size)
	if err != nil {
		return err
	}
	if err := Unmount(mountPoint); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = run("mount", "-o", "loop", imagePath, mountPoint)
		return err
	}
	if err := os.Truncate(imagePath, bytes); err != nil {
		return fail(err)
	}
	if _, err := findBinary("e2fsck"); err != nil {
		return fail(err)
	}
	if err := run("e2fsck", "-fy", imagePath); err != nil {
		return fail(err)
	}
	if _, err := findBinary("resize2fs"); err != nil {
		return fail(err)
	}
	if err := run("resize2fs", imagePath); err != nil {
		return fail(err)
	}
	return run("mount", "-o", "loop", imagePath, mountPoint)
}

func Unmount(path string) error {
	if runtime.GOOS != "linux" || !isMountPoint(path) {
		return nil
	}
	return run("umount", path)
}

func findBinary(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found", name)
}

func ParseSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("size is required")
	}
	multiplier := int64(1)
	suffix := value[len(value)-1]
	switch suffix {
	case 'k', 'K':
		multiplier = 1024
		value = value[:len(value)-1]
	case 'm', 'M':
		multiplier = 1024 * 1024
		value = value[:len(value)-1]
	case 'g', 'G':
		multiplier = 1024 * 1024 * 1024
		value = value[:len(value)-1]
	case 't', 'T':
		multiplier = 1024 * 1024 * 1024 * 1024
		value = value[:len(value)-1]
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", value, err)
	}
	return n * multiplier, nil
}
