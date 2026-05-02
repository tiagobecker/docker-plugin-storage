package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalBackupVerifyAndRestore(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "snapshot.tar.gz")
	target := filepath.Join(root, "backups")
	restore := filepath.Join(root, "restore.tar.gz")
	if err := os.WriteFile(src, []byte("snapshot-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest, err := Put(target, src, "snap1", "vol1")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.BackupID == "" || manifest.DataSHA256 == "" || manifest.DataBytes != int64(len("snapshot-data")) {
		t.Fatalf("bad manifest: %+v", manifest)
	}
	if _, err := Verify(target, manifest.BackupID); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(target, manifest.BackupID, restore); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(restore)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "snapshot-data" {
		t.Fatalf("restore mismatch: %q", string(b))
	}
}

func TestLocalBackupVerifyDetectsCorruption(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "snapshot.tar.gz")
	target := filepath.Join(root, "backups")
	if err := os.WriteFile(src, []byte("snapshot-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Put(target, src, "snap1", "vol1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, manifest.BackupID, DataName), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(target, manifest.BackupID); err == nil {
		t.Fatal("expected corruption to be detected")
	}
}

func TestLocalPutStream(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "backups")
	restore := filepath.Join(root, "restore.tar.gz")

	manifest, err := PutStream(target, strings.NewReader("stream-data"), "stream-backup", "vol1", "unit-test-stream")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Source != "unit-test-stream" {
		t.Fatalf("unexpected source: %q", manifest.Source)
	}
	if _, err := Verify(target, manifest.BackupID); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(target, manifest.BackupID, restore); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(restore)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "stream-data" {
		t.Fatalf("restore mismatch: %q", string(b))
	}
}
