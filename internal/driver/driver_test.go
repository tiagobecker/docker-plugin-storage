package driver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestDriver(t *testing.T, opts Options) *Driver {
	t.Helper()
	if opts.Root == "" {
		opts.Root = filepath.Join(t.TempDir(), "state")
	}
	if opts.MountRoot == "" {
		opts.MountRoot = filepath.Join(t.TempDir(), "mount")
	}
	opts.DisableImageMount = true
	d, err := NewWithOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSnapshotAndRestore(t *testing.T) {
	d := newTestDriver(t, Options{})

	v, err := d.Create("data", map[string]string{"size": "1g", "inodes": "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.Mountpoint, "file.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	sn, err := d.Snapshot("data", "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if sn.SHA256 == "" || sn.ManifestPath == "" {
		t.Fatalf("snapshot should have checksum and manifest: %+v", sn)
	}
	if err := os.WriteFile(filepath.Join(v.Mountpoint, "file.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(sn.Name, ""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(v.Mountpoint, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "before" {
		t.Fatalf("restore mismatch: %q", string(b))
	}
}

func TestCorruptSnapshotRestoreIsRejected(t *testing.T) {
	d := newTestDriver(t, Options{DefaultSize: "1g"})

	v, err := d.Create("data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.Mountpoint, "file.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	sn, err := d.Snapshot("data", "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sn.Path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(sn.Name, ""); err == nil {
		t.Fatal("expected corrupt snapshot restore to be rejected")
	}
}

func TestMountedRestoreIsRejected(t *testing.T) {
	d := newTestDriver(t, Options{DefaultSize: "1g"})

	v, err := d.Create("data", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.Mountpoint, "file.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	sn, err := d.Snapshot("data", "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount("data", "consumer"); err != nil {
		t.Fatal(err)
	}
	if err := d.Restore(sn.Name, "data"); err == nil {
		t.Fatal("expected restore into mounted volume to fail")
	}
}

func TestMountedSnapshotIsRejectedByDefault(t *testing.T) {
	d := newTestDriver(t, Options{DefaultSize: "1g"})

	if _, err := d.Create("data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount("data", "consumer"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Snapshot("data", "snap1"); err == nil {
		t.Fatal("expected mounted snapshot to be rejected")
	}
}

func TestMountedBackupVolumeIsRejectedByDefault(t *testing.T) {
	d := newTestDriver(t, Options{DefaultSize: "1g"})

	if _, err := d.Create("data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount("data", "consumer"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BackupVolume("data", filepath.Join(t.TempDir(), "backup"), "snap1"); err == nil {
		t.Fatal("expected mounted backup-volume to be rejected")
	}
}

func TestCrashConsistentPolicyAllowsMountedSnapshot(t *testing.T) {
	d := newTestDriver(t, Options{
		DefaultSize:   "1g",
		ArchivePolicy: ArchivePolicyCrashConsistent,
	})

	if _, err := d.Create("data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount("data", "consumer"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Snapshot("data", "snap1"); err != nil {
		t.Fatal(err)
	}
}

func TestHookedPolicyRunsHooksForMountedSnapshot(t *testing.T) {
	hookLog := filepath.Join(t.TempDir(), "hooks.log")
	d := newTestDriver(t, Options{
		DefaultSize:     "1g",
		ArchivePolicy:   ArchivePolicyHooked,
		PreArchiveHook:  `printf "$DPS_HOOK_PHASE:$DPS_ARCHIVE_OPERATION:$DPS_ARCHIVE_ARTIFACT:$DPS_VOLUME_NAME\n" >> "` + hookLog + `"`,
		PostArchiveHook: `printf "$DPS_HOOK_PHASE:$DPS_ARCHIVE_OPERATION:$DPS_ARCHIVE_ARTIFACT:$DPS_VOLUME_NAME\n" >> "` + hookLog + `"`,
	})

	if _, err := d.Create("data", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mount("data", "consumer"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Snapshot("data", "snap1"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "pre:snapshot:snap1:data") || !strings.Contains(got, "post:snapshot:snap1:data") {
		t.Fatalf("hook log missing entries: %q", got)
	}
}

func TestHookedPolicyRunsPostHookAfterArchiveFailure(t *testing.T) {
	hookLog := filepath.Join(t.TempDir(), "hooks.log")
	d := newTestDriver(t, Options{
		DefaultSize:     "1g",
		ArchivePolicy:   ArchivePolicyHooked,
		PreArchiveHook:  `printf "$DPS_HOOK_PHASE\n" >> "` + hookLog + `"`,
		PostArchiveHook: `printf "$DPS_HOOK_PHASE\n" >> "` + hookLog + `"`,
	})

	v, err := d.Create("data", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = d.withArchiveConsistency(v, "snapshot", "snap1", func() error {
		return errors.New("archive failed")
	})
	if err == nil || !strings.Contains(err.Error(), "archive failed") {
		t.Fatalf("expected archive failure, got %v", err)
	}
	b, err := os.ReadFile(hookLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "pre\npost\n" {
		t.Fatalf("hook log mismatch: %q", string(b))
	}
}

func TestDefaultLimitsAreApplied(t *testing.T) {
	d := newTestDriver(t, Options{
		DefaultSize:   "10g",
		DefaultInodes: "200000",
	})

	v, err := d.Create("defaulted", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Size != "10g" || v.Inodes != "200000" {
		t.Fatalf("unexpected defaults: size=%q inodes=%q", v.Size, v.Inodes)
	}
}

func TestDriverOptionsOverrideDefaultLimits(t *testing.T) {
	d := newTestDriver(t, Options{
		DefaultSize:   "10g",
		DefaultInodes: "200000",
	})

	v, err := d.Create("custom", map[string]string{"size": "1g", "inodes": "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Size != "1g" || v.Inodes != "1000" {
		t.Fatalf("driver opts should override defaults: size=%q inodes=%q", v.Size, v.Inodes)
	}
}

func TestCreateRejectsVolumeWithoutSize(t *testing.T) {
	d := newTestDriver(t, Options{})

	if _, err := d.Create("unbounded", nil); err == nil {
		t.Fatal("expected volume without size to be rejected")
	}
}

func TestCreateAcceptsDefaultSize(t *testing.T) {
	d := newTestDriver(t, Options{
		DefaultSize: "5g",
	})

	v, err := d.Create("bounded", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Size != "5g" {
		t.Fatalf("unexpected default size: %q", v.Size)
	}
}
