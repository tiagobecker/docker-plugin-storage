package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/devpower/dps/internal/archive"
	"github.com/devpower/dps/internal/backup"
	"github.com/devpower/dps/internal/imagefs"
	"github.com/devpower/dps/internal/store"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

const (
	ArchivePolicyOffline         = "offline"
	ArchivePolicyCrashConsistent = "crash-consistent"
	ArchivePolicyHooked          = "hooked"
	defaultArchiveHookTimeout    = 10 * time.Minute
)

type snapshotManifest struct {
	Version    int       `json:"version"`
	Name       string    `json:"name"`
	Volume     string    `json:"volume"`
	CreatedAt  time.Time `json:"created_at"`
	Format     string    `json:"format"`
	DataObject string    `json:"data_object"`
	DataBytes  int64     `json:"data_bytes"`
	DataSHA256 string    `json:"data_sha256"`
}

type Driver struct {
	Root               string
	MountRoot          string
	ImageRoot          string
	DefaultSize        string
	DefaultInodes      string
	ArchivePolicy      string
	PreArchiveHook     string
	PostArchiveHook    string
	ArchiveHookTimeout time.Duration
	Store              *store.Store
	DisableImageMount  bool
}

type Options struct {
	Root               string
	MountRoot          string
	ImageRoot          string
	DefaultSize        string
	DefaultInodes      string
	ArchivePolicy      string
	PreArchiveHook     string
	PostArchiveHook    string
	ArchiveHookTimeout time.Duration
	DisableImageMount  bool
}

func New(root, mountRoot string) (*Driver, error) {
	return NewWithOptions(Options{Root: root, MountRoot: mountRoot})
}

func NewWithOptions(opts Options) (*Driver, error) {
	root := opts.Root
	if root == "" {
		root = "/var/lib/dps"
	}
	mountRoot := opts.MountRoot
	if mountRoot == "" {
		mountRoot = "/mnt/dps"
	}
	imageRoot := opts.ImageRoot
	if imageRoot == "" {
		imageRoot = filepath.Join(root, "volume-images")
	}
	st, err := store.Open(filepath.Join(root, "metadata.json"))
	if err != nil {
		return nil, err
	}
	archivePolicy, err := normalizeArchivePolicy(opts.ArchivePolicy)
	if err != nil {
		return nil, err
	}
	hookTimeout := opts.ArchiveHookTimeout
	if hookTimeout == 0 {
		hookTimeout = defaultArchiveHookTimeout
	}
	if hookTimeout < 0 {
		return nil, errors.New("archive hook timeout must be positive")
	}
	d := &Driver{
		Root:               root,
		MountRoot:          mountRoot,
		ImageRoot:          imageRoot,
		DefaultSize:        opts.DefaultSize,
		DefaultInodes:      opts.DefaultInodes,
		ArchivePolicy:      archivePolicy,
		PreArchiveHook:     opts.PreArchiveHook,
		PostArchiveHook:    opts.PostArchiveHook,
		ArchiveHookTimeout: hookTimeout,
		Store:              st,
		DisableImageMount:  opts.DisableImageMount,
	}
	for _, dir := range []string{d.volumeRoot(), d.snapshotRoot(), d.ImageRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (d *Driver) Create(name string, opts map[string]string) (*store.Volume, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	opts = d.applyDefaultLimits(opts)
	if err := d.validateLimits(opts); err != nil {
		return nil, err
	}
	mp := filepath.Join(d.volumeRoot(), name)
	if err := os.MkdirAll(mp, 0o700); err != nil {
		return nil, err
	}
	v, err := d.Store.CreateVolume(name, mp, opts)
	if err != nil {
		return nil, err
	}
	if err := d.ensureVolumeBacking(v); err != nil {
		_ = d.Store.DeleteVolume(name)
		_ = safeRemoveAll(d.volumeRoot(), mp)
		return nil, err
	}
	return v, nil
}

func (d *Driver) applyDefaultLimits(opts map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range opts {
		out[k] = v
	}
	if out["size"] == "" && d.DefaultSize != "" {
		out["size"] = d.DefaultSize
	}
	if out["inodes"] == "" && d.DefaultInodes != "" {
		out["inodes"] = d.DefaultInodes
	}
	return out
}

func (d *Driver) validateLimits(opts map[string]string) error {
	if opts["size"] == "" {
		return errors.New("volume size limit is required; set driver_opts.size or DPS_DEFAULT_VOLUME_SIZE")
	}
	return nil
}

func (d *Driver) Remove(name string) error {
	v, ok := d.Store.GetVolume(name)
	if !ok {
		return nil
	}
	if v.RefCount > 0 {
		return fmt.Errorf("volume %s is mounted by %d consumer(s)", name, v.RefCount)
	}
	if err := imagefs.Unmount(v.Mountpoint); err != nil {
		return err
	}
	if err := safeRemoveAll(d.volumeRoot(), v.Mountpoint); err != nil {
		return err
	}
	_ = os.Remove(d.volumeImagePath(name))
	return d.Store.DeleteVolume(name)
}

func (d *Driver) Mount(name, id string) (*store.Volume, error) {
	v, ok := d.Store.GetVolume(name)
	if !ok {
		var err error
		v, err = d.Create(name, map[string]string{})
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(v.Mountpoint, 0o700); err != nil {
		return nil, err
	}
	if err := d.ensureVolumeBacking(v); err != nil {
		return nil, err
	}
	v.RefCount++
	if err := d.Store.UpdateVolume(v); err != nil {
		return nil, err
	}
	return v, nil
}

func (d *Driver) Unmount(name, id string) error {
	v, ok := d.Store.GetVolume(name)
	if !ok {
		return nil
	}
	if v.RefCount > 0 {
		v.RefCount--
	}
	return d.Store.UpdateVolume(v)
}

func (d *Driver) Resize(name, size, inodes string) error {
	v, ok := d.Store.GetVolume(name)
	if !ok {
		return os.ErrNotExist
	}
	oldSize := v.Size
	oldInodes := v.Inodes
	if size != "" {
		v.Size = size
	}
	if inodes != "" {
		v.Inodes = inodes
	}
	if v.RefCount > 0 {
		return fmt.Errorf("offline resize is required for volume %s", name)
	}
	if err := d.resizeImageVolume(v, oldSize, oldInodes); err != nil {
		return err
	}
	return d.Store.UpdateVolume(v)
}

func (d *Driver) Snapshot(volume, name string) (*store.Snapshot, error) {
	v, ok := d.Store.GetVolume(volume)
	if !ok {
		return nil, os.ErrNotExist
	}
	if name == "" {
		name = fmt.Sprintf("%s-%s", volume, time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := validateName(name); err != nil {
		return nil, err
	}
	dst := filepath.Join(d.snapshotRoot(), volume, name+".tar.gz")
	var bytes int64
	var sum string
	if err := d.withArchiveConsistency(v, "snapshot", name, func() error {
		var err error
		bytes, sum, err = archive.CreateTarGzAtomic(v.Mountpoint, dst)
		return err
	}); err != nil {
		_ = os.Remove(dst)
		return nil, err
	}
	createdAt := time.Now().UTC()
	manifestPath := dst + ".manifest.json"
	manifest := snapshotManifest{
		Version:    1,
		Name:       name,
		Volume:     volume,
		CreatedAt:  createdAt,
		Format:     "tar.gz",
		DataObject: filepath.Base(dst),
		DataBytes:  bytes,
		DataSHA256: sum,
	}
	if err := writeJSONAtomic(manifestPath, manifest); err != nil {
		_ = os.Remove(dst)
		return nil, err
	}
	sn := &store.Snapshot{
		Name:         name,
		Volume:       volume,
		Path:         dst,
		ManifestPath: manifestPath,
		Bytes:        bytes,
		SHA256:       sum,
		Format:       "tar.gz",
		CreatedAt:    createdAt,
	}
	if err := d.Store.AddSnapshot(sn); err != nil {
		_ = os.Remove(dst)
		_ = os.Remove(manifestPath)
		return nil, err
	}
	return sn, nil
}

func (d *Driver) Restore(snapshotName, volume string) error {
	sn, ok := d.Store.GetSnapshot(snapshotName)
	if !ok {
		return os.ErrNotExist
	}
	if volume == "" {
		volume = sn.Volume
	}
	v, ok := d.Store.GetVolume(volume)
	if !ok {
		var err error
		v, err = d.Create(volume, map[string]string{})
		if err != nil {
			return err
		}
	}
	if v.RefCount > 0 {
		return fmt.Errorf("refusing to restore into mounted volume %s", volume)
	}
	if err := verifySnapshot(sn); err != nil {
		return err
	}
	if err := safeRemoveContents(v.Mountpoint); err != nil {
		return err
	}
	return archive.ExtractTarGz(sn.Path, v.Mountpoint)
}

func (d *Driver) Backup(snapshotName, target string) (*backup.Manifest, error) {
	sn, ok := d.Store.GetSnapshot(snapshotName)
	if !ok {
		return nil, os.ErrNotExist
	}
	if err := verifySnapshot(sn); err != nil {
		return nil, err
	}
	return backup.Put(target, sn.Path, sn.Name, sn.Volume)
}

func (d *Driver) BackupVolume(volume, target, snapshotName string) (*backup.Manifest, error) {
	v, ok := d.Store.GetVolume(volume)
	if !ok {
		return nil, os.ErrNotExist
	}
	backupName := snapshotName
	if backupName == "" {
		backupName = fmt.Sprintf("%s-stream", volume)
	}
	var manifest *backup.Manifest
	err := d.withArchiveConsistency(v, "backup-volume", backupName, func() error {
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(archive.WriteTarGz(v.Mountpoint, pw))
		}()
		var err error
		manifest, err = backup.PutStream(target, pr, backupName, volume, "tar-stream")
		return err
	})
	return manifest, err
}

func (d *Driver) VerifyBackup(target, backupID string) (*backup.Manifest, error) {
	return backup.Verify(target, backupID)
}

func (d *Driver) RestoreBackup(target, backupID, volume string) error {
	tmp := filepath.Join(d.Root, "tmp", "restore-"+safePathToken(backupID)+".tar.gz")
	manifest, err := backup.Restore(target, backupID, tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if volume == "" {
		volume = manifest.Volume
	}
	if volume == "" {
		return errors.New("restore volume is required because backup manifest has no volume")
	}
	v, ok := d.Store.GetVolume(volume)
	if !ok {
		v, err = d.Create(volume, map[string]string{})
		if err != nil {
			return err
		}
	}
	if v.RefCount > 0 {
		return fmt.Errorf("refusing to restore into mounted volume %s", volume)
	}
	if err := safeRemoveContents(v.Mountpoint); err != nil {
		return err
	}
	return archive.ExtractTarGz(tmp, v.Mountpoint)
}

func (d *Driver) volumeRoot() string {
	return filepath.Join(d.MountRoot, "volumes")
}

func (d *Driver) snapshotRoot() string {
	return filepath.Join(d.Root, "snapshots")
}

func (d *Driver) volumeImagePath(name string) string {
	return filepath.Join(d.ImageRoot, name+".img")
}

func (d *Driver) ensureVolumeBacking(v *store.Volume) error {
	if d.DisableImageMount {
		return os.MkdirAll(v.Mountpoint, 0o700)
	}
	return imagefs.Ensure(d.volumeImagePath(v.Name), v.Mountpoint, v.Size, v.Inodes)
}

func (d *Driver) resizeImageVolume(v *store.Volume, oldSize, oldInodes string) error {
	if v.Size == "" {
		return errors.New("volume resize requires a size")
	}
	if err := d.ensureVolumeBacking(v); err != nil {
		return err
	}
	if err := d.validateResizeFits(v.Mountpoint, oldSize, oldInodes, v.Size, v.Inodes); err != nil {
		return err
	}

	recreate := mustRecreateImage(oldSize, oldInodes, v.Size, v.Inodes)
	if !recreate {
		if err := imagefs.Grow(d.volumeImagePath(v.Name), v.Mountpoint, v.Size); err == nil {
			return nil
		}
	}
	return d.recreateImageVolume(v)
}

func mustRecreateImage(oldSize, oldInodes, newSize, newInodes string) bool {
	if newInodes != "" && newInodes != oldInodes {
		return true
	}
	oldBytes, oldErr := imagefs.ParseSize(oldSize)
	newBytes, newErr := imagefs.ParseSize(newSize)
	if oldErr != nil || newErr != nil {
		return true
	}
	return newBytes < oldBytes
}

func (d *Driver) recreateImageVolume(v *store.Volume) error {
	tmpSnapshot := filepath.Join(d.Root, "tmp", "resize-"+v.Name+"-"+time.Now().UTC().Format("20060102T150405Z")+".tar.gz")
	if _, err := archive.CreateTarGz(v.Mountpoint, tmpSnapshot); err != nil {
		return err
	}
	defer os.Remove(tmpSnapshot)

	image := d.volumeImagePath(v.Name)
	backupImage := image + ".resize-backup"
	_ = os.Remove(backupImage)

	if err := imagefs.Unmount(v.Mountpoint); err != nil {
		return err
	}
	if err := os.Rename(image, backupImage); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := safeRemoveContents(v.Mountpoint); err != nil {
		_ = restoreImageBackup(backupImage, image, v.Mountpoint)
		return err
	}
	if err := imagefs.Ensure(image, v.Mountpoint, v.Size, v.Inodes); err != nil {
		_ = restoreImageBackup(backupImage, image, v.Mountpoint)
		return err
	}
	if err := archive.ExtractTarGz(tmpSnapshot, v.Mountpoint); err != nil {
		_ = imagefs.Unmount(v.Mountpoint)
		_ = os.Remove(image)
		_ = restoreImageBackup(backupImage, image, v.Mountpoint)
		return err
	}
	_ = os.Remove(backupImage)
	return nil
}

func restoreImageBackup(backupImage, image, mountpoint string) error {
	_ = imagefs.Unmount(mountpoint)
	_ = os.Remove(image)
	if err := os.Rename(backupImage, image); err != nil {
		return err
	}
	return imagefs.Ensure(image, mountpoint, "1", "")
}

func (d *Driver) validateResizeFits(path, oldSize, oldInodes, newSize, newInodes string) error {
	usage, err := measureUsage(path)
	if err != nil {
		return err
	}
	if newSize != "" && isShrinkSize(oldSize, newSize) {
		target, err := imagefs.ParseSize(newSize)
		if err != nil {
			return err
		}
		if usage.Bytes > target*90/100 {
			return fmt.Errorf("volume uses %d bytes; refusing to shrink to %s without at least 10%% headroom", usage.Bytes, newSize)
		}
	}
	if newInodes != "" && isShrinkInodes(oldInodes, newInodes) {
		target, err := strconv.ParseInt(newInodes, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid inode limit %q: %w", newInodes, err)
		}
		if usage.Inodes > target*90/100 {
			return fmt.Errorf("volume uses %d inodes; refusing to shrink to %s without at least 10%% headroom", usage.Inodes, newInodes)
		}
	}
	return nil
}

type volumeUsage struct {
	Bytes  int64
	Inodes int64
}

func measureUsage(root string) (volumeUsage, error) {
	var usage volumeUsage
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		usage.Inodes++
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
			usage.Bytes += st.Blocks * 512
		} else if info.Mode().IsRegular() {
			usage.Bytes += info.Size()
		}
		return nil
	})
	return usage, err
}

func isShrinkSize(oldSize, newSize string) bool {
	oldBytes, oldErr := imagefs.ParseSize(oldSize)
	newBytes, newErr := imagefs.ParseSize(newSize)
	return oldErr == nil && newErr == nil && newBytes < oldBytes
}

func isShrinkInodes(oldInodes, newInodes string) bool {
	oldValue, oldErr := strconv.ParseInt(oldInodes, 10, 64)
	newValue, newErr := strconv.ParseInt(newInodes, 10, 64)
	return oldErr == nil && newErr == nil && newValue < oldValue
}

func validateName(name string) error {
	if !safeName.MatchString(name) {
		return fmt.Errorf("invalid name %q", name)
	}
	return nil
}

func safePathToken(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "backup"
	}
	return out
}

func normalizeArchivePolicy(policy string) (string, error) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		return ArchivePolicyOffline, nil
	}
	switch policy {
	case ArchivePolicyOffline:
		return policy, nil
	case ArchivePolicyCrashConsistent, "crash", "mounted-explicit":
		return ArchivePolicyCrashConsistent, nil
	case ArchivePolicyHooked:
		return policy, nil
	default:
		return "", fmt.Errorf("unknown archive policy %q; expected offline, crash-consistent, or hooked", policy)
	}
}

func (d *Driver) withArchiveConsistency(v *store.Volume, operation, artifact string, archiveFn func() error) error {
	switch d.ArchivePolicy {
	case ArchivePolicyOffline:
		if v.RefCount > 0 {
			return fmt.Errorf("refusing to %s mounted volume %s with archive policy %s", operation, v.Name, d.ArchivePolicy)
		}
		return archiveFn()
	case ArchivePolicyCrashConsistent:
		return archiveFn()
	case ArchivePolicyHooked:
		if d.PreArchiveHook == "" || d.PostArchiveHook == "" {
			return errors.New("archive policy hooked requires both pre and post archive hooks")
		}
		if err := d.runArchiveHook("pre", operation, artifact, v); err != nil {
			return err
		}
		archiveErr := archiveFn()
		postErr := d.runArchiveHook("post", operation, artifact, v)
		if archiveErr != nil || postErr != nil {
			return errors.Join(archiveErr, postErr)
		}
		return nil
	default:
		return fmt.Errorf("unknown archive policy %q", d.ArchivePolicy)
	}
}

func (d *Driver) runArchiveHook(phase, operation, artifact string, v *store.Volume) error {
	hook := d.PreArchiveHook
	if phase == "post" {
		hook = d.PostArchiveHook
	}
	if hook == "" {
		return nil
	}
	timeout := d.ArchiveHookTimeout
	if timeout == 0 {
		timeout = defaultArchiveHookTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", hook)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{Buffer: &stdout, Limit: 16 * 1024}
	cmd.Stderr = &limitedBuffer{Buffer: &stderr, Limit: 16 * 1024}
	cmd.Env = append(os.Environ(),
		"DPS_HOOK_PHASE="+phase,
		"DPS_ARCHIVE_OPERATION="+operation,
		"DPS_ARCHIVE_POLICY="+d.ArchivePolicy,
		"DPS_ARCHIVE_ARTIFACT="+artifact,
		"DPS_VOLUME_NAME="+v.Name,
		"DPS_VOLUME_MOUNTPOINT="+v.Mountpoint,
		"DPS_VOLUME_REFCOUNT="+strconv.Itoa(v.RefCount),
	)
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s archive hook timed out after %s", phase, timeout)
		}
		output := strings.TrimSpace(stdout.String() + stderr.String())
		if output != "" {
			return fmt.Errorf("%s archive hook failed: %w: %s", phase, err, output)
		}
		return fmt.Errorf("%s archive hook failed: %w", phase, err)
	}
	return nil
}

type limitedBuffer struct {
	*bytes.Buffer
	Limit int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if w.Buffer.Len() < w.Limit {
		remaining := w.Limit - w.Buffer.Len()
		if len(p) <= remaining {
			_, _ = w.Buffer.Write(p)
		} else {
			_, _ = w.Buffer.Write(p[:remaining])
		}
	}
	return len(p), nil
}

func verifySnapshot(sn *store.Snapshot) error {
	if sn == nil {
		return os.ErrNotExist
	}
	bytes, sum, err := archive.HashFile(sn.Path)
	if err != nil {
		return err
	}
	if sn.Bytes > 0 && bytes != sn.Bytes {
		return fmt.Errorf("snapshot %s size mismatch: catalog=%d actual=%d", sn.Name, sn.Bytes, bytes)
	}
	if sn.SHA256 != "" && sum != sn.SHA256 {
		return fmt.Errorf("snapshot %s checksum mismatch: catalog=%s actual=%s", sn.Name, sn.SHA256, sum)
	}
	if sn.ManifestPath != "" {
		var manifest snapshotManifest
		if err := readJSON(sn.ManifestPath, &manifest); err != nil {
			return err
		}
		if manifest.DataBytes != bytes {
			return fmt.Errorf("snapshot %s manifest size mismatch: manifest=%d actual=%d", sn.Name, manifest.DataBytes, bytes)
		}
		if manifest.DataSHA256 != sum {
			return fmt.Errorf("snapshot %s manifest checksum mismatch: manifest=%s actual=%s", sn.Name, manifest.DataSHA256, sum)
		}
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func safeRemoveAll(root, target string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator) {
		return errors.New("refusing to remove path outside volume root")
	}
	return os.RemoveAll(targetAbs)
}

func safeRemoveContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
