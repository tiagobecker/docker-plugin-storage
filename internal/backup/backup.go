package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ManifestName = "manifest.json"
	DataName     = "data.tar.gz"
)

type Manifest struct {
	Version      int       `json:"version"`
	BackupID     string    `json:"backup_id"`
	CreatedAt    time.Time `json:"created_at"`
	SnapshotName string    `json:"snapshot_name"`
	Volume       string    `json:"volume"`
	Format       string    `json:"format"`
	Source       string    `json:"source"`
	DataObject   string    `json:"data_object"`
	DataBytes    int64     `json:"data_bytes"`
	DataSHA256   string    `json:"data_sha256"`
}

func Put(target, sourceFile, snapshotName, volume string) (*Manifest, error) {
	if target == "" {
		return nil, errors.New("backup target is required")
	}
	backupID := safeID(firstNonEmpty(snapshotName, filepath.Base(sourceFile))) + "-" + time.Now().UTC().Format("20060102T150405Z")
	manifest := &Manifest{
		Version:      1,
		BackupID:     backupID,
		CreatedAt:    time.Now().UTC(),
		SnapshotName: snapshotName,
		Volume:       volume,
		Format:       "tar.gz",
		Source:       "snapshot-file",
		DataObject:   DataName,
	}

	if strings.HasPrefix(target, "s3://") {
		return putS3(target, sourceFile, manifest)
	}
	return putLocal(target, sourceFile, manifest)
}

func PutStream(target string, src io.Reader, backupName, volume, source string) (*Manifest, error) {
	if target == "" {
		return nil, errors.New("backup target is required")
	}
	backupID := safeID(backupName) + "-" + time.Now().UTC().Format("20060102T150405Z")
	manifest := &Manifest{
		Version:    1,
		BackupID:   backupID,
		CreatedAt:  time.Now().UTC(),
		Volume:     volume,
		Format:     "tar.gz",
		Source:     firstNonEmpty(source, "stream"),
		DataObject: DataName,
	}
	if strings.HasPrefix(target, "s3://") {
		return putS3Stream(target, src, manifest)
	}
	return putLocalStream(target, src, manifest)
}

func Verify(target, backupID string) (*Manifest, error) {
	if strings.HasPrefix(target, "s3://") {
		manifest, client, prefix, err := loadS3Manifest(target, backupID)
		if err != nil {
			return nil, err
		}
		sum, bytes, err := client.HashObject(prefix + "/" + manifest.DataObject)
		if err != nil {
			return nil, err
		}
		return manifest, validateManifestData(manifest, sum, bytes)
	}

	manifest, err := loadLocalManifest(target, backupID)
	if err != nil {
		return nil, err
	}
	dataPath := filepath.Join(target, backupID, manifest.DataObject)
	sum, bytes, err := hashFile(dataPath)
	if err != nil {
		return nil, err
	}
	return manifest, validateManifestData(manifest, sum, bytes)
}

func Restore(target, backupID, dstFile string) (*Manifest, error) {
	if dstFile == "" {
		return nil, errors.New("restore destination file is required")
	}
	if strings.HasPrefix(target, "s3://") {
		manifest, client, prefix, err := loadS3Manifest(target, backupID)
		if err != nil {
			return nil, err
		}
		sum, bytes, err := client.GetObjectToFile(prefix+"/"+manifest.DataObject, dstFile)
		if err != nil {
			return nil, err
		}
		return manifest, validateManifestData(manifest, sum, bytes)
	}

	manifest, err := loadLocalManifest(target, backupID)
	if err != nil {
		return nil, err
	}
	dataPath := filepath.Join(target, backupID, manifest.DataObject)
	sum, bytes, err := copyFileAtomic(dataPath, dstFile)
	if err != nil {
		return nil, err
	}
	return manifest, validateManifestData(manifest, sum, bytes)
}

func putLocal(target, sourceFile string, manifest *Manifest) (*Manifest, error) {
	backupDir := filepath.Join(target, manifest.BackupID)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return nil, err
	}
	dataPath := filepath.Join(backupDir, manifest.DataObject)
	sum, bytes, err := copyFileAtomic(sourceFile, dataPath)
	if err != nil {
		return nil, err
	}
	manifest.DataSHA256 = sum
	manifest.DataBytes = bytes
	if err := writeManifestAtomic(filepath.Join(backupDir, ManifestName), manifest); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(backupDir, ManifestName+".sha256"), []byte(manifestSHA256Line(manifest)), 0o600); err != nil {
		return nil, err
	}
	return Verify(target, manifest.BackupID)
}

func putLocalStream(target string, src io.Reader, manifest *Manifest) (*Manifest, error) {
	backupDir := filepath.Join(target, manifest.BackupID)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return nil, err
	}
	dataPath := filepath.Join(backupDir, manifest.DataObject)
	sum, bytes, err := writeStreamAtomic(src, dataPath)
	if err != nil {
		return nil, err
	}
	manifest.DataSHA256 = sum
	manifest.DataBytes = bytes
	if err := writeManifestAtomic(filepath.Join(backupDir, ManifestName), manifest); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(backupDir, ManifestName+".sha256"), []byte(manifestSHA256Line(manifest)), 0o600); err != nil {
		return nil, err
	}
	return Verify(target, manifest.BackupID)
}

func putS3(target, sourceFile string, manifest *Manifest) (*Manifest, error) {
	client, prefix, err := s3ClientForTarget(target)
	if err != nil {
		return nil, err
	}
	backupPrefix := strings.Trim(prefix+"/"+manifest.BackupID, "/")
	dataKey := backupPrefix + "/" + manifest.DataObject
	sum, bytes, err := client.PutObjectFile(dataKey, sourceFile)
	if err != nil {
		return nil, err
	}
	manifest.DataSHA256 = sum
	manifest.DataBytes = bytes
	readBackSum, readBackBytes, err := client.HashObject(dataKey)
	if err != nil {
		return nil, err
	}
	if err := validateManifestData(manifest, readBackSum, readBackBytes); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := client.PutObject(backupPrefix+"/"+ManifestName, body); err != nil {
		return nil, err
	}
	if err := client.PutObject(backupPrefix+"/"+ManifestName+".sha256", []byte(manifestSHA256Line(manifest))); err != nil {
		return nil, err
	}
	return Verify(target, manifest.BackupID)
}

func putS3Stream(target string, src io.Reader, manifest *Manifest) (*Manifest, error) {
	client, prefix, err := s3ClientForTarget(target)
	if err != nil {
		return nil, err
	}
	backupPrefix := strings.Trim(prefix+"/"+manifest.BackupID, "/")
	dataKey := backupPrefix + "/" + manifest.DataObject
	sum, bytes, err := client.PutObjectStreamMultipart(dataKey, src)
	if err != nil {
		return nil, err
	}
	manifest.DataSHA256 = sum
	manifest.DataBytes = bytes
	readBackSum, readBackBytes, err := client.HashObject(dataKey)
	if err != nil {
		return nil, err
	}
	if err := validateManifestData(manifest, readBackSum, readBackBytes); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := client.PutObject(backupPrefix+"/"+ManifestName, body); err != nil {
		return nil, err
	}
	if err := client.PutObject(backupPrefix+"/"+ManifestName+".sha256", []byte(manifestSHA256Line(manifest))); err != nil {
		return nil, err
	}
	return Verify(target, manifest.BackupID)
}

func loadLocalManifest(target, backupID string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(target, backupID, ManifestName))
	if err != nil {
		return nil, err
	}
	return decodeManifest(b)
}

func loadS3Manifest(target, backupID string) (*Manifest, *S3Client, string, error) {
	client, prefix, err := s3ClientForTarget(target)
	if err != nil {
		return nil, nil, "", err
	}
	backupPrefix := strings.Trim(prefix+"/"+backupID, "/")
	b, err := client.GetObject(backupPrefix + "/" + ManifestName)
	if err != nil {
		return nil, nil, "", err
	}
	manifest, err := decodeManifest(b)
	return manifest, client, backupPrefix, err
}

func decodeManifest(b []byte) (*Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		return nil, err
	}
	if manifest.Version != 1 || manifest.BackupID == "" || manifest.DataObject == "" || manifest.DataSHA256 == "" {
		return nil, errors.New("invalid backup manifest")
	}
	return &manifest, nil
}

func validateManifestData(manifest *Manifest, sum string, bytes int64) error {
	if manifest.DataSHA256 != sum {
		return fmt.Errorf("backup checksum mismatch: manifest=%s actual=%s", manifest.DataSHA256, sum)
	}
	if manifest.DataBytes != bytes {
		return fmt.Errorf("backup size mismatch: manifest=%d actual=%d", manifest.DataBytes, bytes)
	}
	return nil
}

func s3ClientForTarget(target string) (*S3Client, string, error) {
	cfg := LoadS3FromEnv()
	raw := strings.TrimPrefix(target, "s3://")
	parts := strings.SplitN(raw, "/", 2)
	cfg.Bucket = parts[0]
	prefix := ""
	if len(parts) == 2 {
		prefix = strings.Trim(parts[1], "/")
	}
	client, err := NewS3Client(cfg)
	return client, prefix, err
}

func copyFileAtomic(src, dst string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	tmp := dst + ".tmp"
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	sum, bytes, copyErr := copyHash(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if syncErr != nil {
		return "", 0, syncErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", 0, err
	}
	return sum, bytes, nil
}

func writeStreamAtomic(src io.Reader, dst string) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	sum, bytes, copyErr := copyHash(out, src)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if syncErr != nil {
		return "", 0, syncErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", 0, err
	}
	return sum, bytes, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return copyHash(io.Discard, f)
}

func copyHash(dst io.Writer, src io.Reader) (string, int64, error) {
	h := sha256.New()
	w := io.MultiWriter(dst, h)
	bytes, err := io.Copy(w, src)
	return hex.EncodeToString(h.Sum(nil)), bytes, err
}

func writeManifestAtomic(path string, manifest *Manifest) error {
	b, err := json.MarshalIndent(manifest, "", "  ")
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

func manifestSHA256Line(manifest *Manifest) string {
	b, _ := json.Marshal(manifest)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]) + "  " + ManifestName + "\n"
}

func safeID(value string) string {
	value = strings.TrimSuffix(filepath.Base(value), filepath.Ext(value))
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
