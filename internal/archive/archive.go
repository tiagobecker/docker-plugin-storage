package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func CreateTarGz(srcDir, dstFile string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dstFile), 0o700); err != nil {
		return 0, err
	}
	f, err := os.Create(dstFile)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcDir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gw.Close(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(dstFile)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func CreateTarGzAtomic(srcDir, dstFile string) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(dstFile), 0o700); err != nil {
		return 0, "", err
	}
	tmp := dstFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if err := WriteTarGz(srcDir, w); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return 0, "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return 0, "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, "", err
	}
	st, err := os.Stat(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return 0, "", err
	}
	if err := os.Rename(tmp, dstFile); err != nil {
		_ = os.Remove(tmp)
		return 0, "", err
	}
	return st.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

func HashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	bytes, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return bytes, hex.EncodeToString(h.Sum(nil)), nil
}

func WriteTarGz(srcDir string, dst io.Writer) error {
	gw := gzip.NewWriter(dst)
	tw := tar.NewWriter(gw)

	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcDir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if err != nil {
		tw.Close()
		gw.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		gw.Close()
		return err
	}
	return gw.Close()
}

func ExtractTarGz(srcFile, dstDir string) error {
	f, err := os.Open(srcFile)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	cleanRoot, err := filepath.Abs(dstDir)
	if err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Name == "" || strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "..") {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		target := filepath.Join(cleanRoot, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) && target != cleanRoot {
			return fmt.Errorf("archive path escapes destination: %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(h.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			_ = os.Chtimes(target, time.Now(), h.ModTime)
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}
}
