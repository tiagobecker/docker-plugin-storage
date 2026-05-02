package imagefs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func Ensure(imagePath, mountPoint, size, inodes string) error {
	if runtime.GOOS != "linux" {
		return os.MkdirAll(mountPoint, 0o700)
	}
	if size == "" {
		return errors.New("volume image size is required")
	}
	if err := requireCommands("mkfs.ext4", "mount"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(mountPoint, 0o700); err != nil {
		return err
	}
	if isMountPoint(mountPoint) {
		return nil
	}
	created, err := ensureSparseFile(imagePath, size)
	if err != nil {
		return err
	}
	if created {
		args := []string{"-F", "-q"}
		if inodes != "" {
			args = append(args, "-N", inodes)
		}
		args = append(args, imagePath)
		if err := run("mkfs.ext4", args...); err != nil {
			return err
		}
	}
	return mountImage(imagePath, mountPoint)
}

func Grow(imagePath, mountPoint, size string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if size == "" {
		return errors.New("volume image size is required")
	}
	if err := requireCommands("e2fsck", "mount", "resize2fs", "umount"); err != nil {
		return err
	}
	bytes, err := ParseSize(size)
	if err != nil {
		return err
	}
	if err := Unmount(mountPoint); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = mountImage(imagePath, mountPoint)
		return err
	}
	if err := os.Truncate(imagePath, bytes); err != nil {
		return fail(err)
	}
	if err := run("e2fsck", "-fy", imagePath); err != nil {
		return fail(err)
	}
	if err := run("resize2fs", imagePath); err != nil {
		return fail(err)
	}
	return mountImage(imagePath, mountPoint)
}

func Unmount(path string) error {
	if runtime.GOOS != "linux" || !isMountPoint(path) {
		return nil
	}
	return run("umount", path)
}

func ParseSize(value string) (int64, error) {
	s := strings.TrimSpace(strings.ToLower(value))
	if s == "" {
		return 0, errors.New("empty size")
	}
	multiplier := int64(1)
	for _, suffix := range []struct {
		s string
		m int64
	}{
		{"kib", 1024},
		{"kb", 1000},
		{"k", 1024},
		{"mib", 1024 * 1024},
		{"mb", 1000 * 1000},
		{"m", 1024 * 1024},
		{"gib", 1024 * 1024 * 1024},
		{"gb", 1000 * 1000 * 1000},
		{"g", 1024 * 1024 * 1024},
		{"tib", 1024 * 1024 * 1024 * 1024},
		{"tb", 1000 * 1000 * 1000 * 1000},
		{"t", 1024 * 1024 * 1024 * 1024},
	} {
		if strings.HasSuffix(s, suffix.s) {
			multiplier = suffix.m
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix.s))
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", value, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid size %q: must be positive", value)
	}
	if n > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("invalid size %q: overflows int64", value)
	}
	return n * multiplier, nil
}

func mountImage(imagePath, mountPoint string) error {
	return run("mount", "-o", "loop,noatime", imagePath, mountPoint)
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

func isMountPoint(path string) bool {
	target, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		target = filepath.Clean(path)
	}
	out, err := exec.Command("findmnt", "-rn", "--target", target, "--output", "TARGET").Output()
	if err == nil && strings.TrimSpace(string(out)) == target {
		return true
	}
	info, err := os.Stat(target)
	if err != nil {
		return false
	}
	parent := filepath.Dir(target)
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	parentSt, parentOK := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || !parentOK {
		return false
	}
	return st.Dev != parentSt.Dev || st.Ino == parentSt.Ino
}

func requireCommands(names ...string) error {
	for _, name := range names {
		if _, err := findBinary(name); err != nil {
			return err
		}
	}
	return nil
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

func findBinary(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, path := range []string{"/usr/sbin/" + name, "/sbin/" + name, "/usr/bin/" + name, "/bin/" + name} {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil
		}
	}
	return "", fmt.Errorf("required command not found: %s", name)
}
