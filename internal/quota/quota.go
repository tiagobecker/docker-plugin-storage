package quota

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type Manager struct {
	MountRoot string
	DryRun    bool
}

func (m Manager) Apply(path string, projectID uint32, size, inodes string) error {
	if size == "" && inodes == "" {
		return nil
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	if m.MountRoot == "" {
		return errors.New("quota mount root is required")
	}
	xfsQuota, err := findXFSQuota()
	if err != nil {
		return err
	}

	project := fmt.Sprintf("%d", projectID)
	setup := fmt.Sprintf("project -s -p %s %s", shellToken(path), project)
	if err := m.run(xfsQuota, setup); err != nil {
		return err
	}

	parts := []string{"limit", "-p"}
	if size != "" {
		parts = append(parts, "bhard="+size)
	}
	if inodes != "" {
		parts = append(parts, "ihard="+inodes)
	}
	parts = append(parts, project)
	return m.run(xfsQuota, strings.Join(parts, " "))
}

func (m Manager) Clear(projectID uint32) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := exec.LookPath("xfs_quota"); err != nil {
		if _, err := os.Stat("/usr/sbin/xfs_quota"); err != nil {
			return nil
		}
	}
	project := fmt.Sprintf("%d", projectID)
	xfsQuota, err := findXFSQuota()
	if err != nil {
		return nil
	}
	return m.run(xfsQuota, fmt.Sprintf("limit -p bhard=0 ihard=0 %s", project))
}

func (m Manager) run(binary, command string) error {
	if m.DryRun {
		return nil
	}
	cmd := exec.Command(binary, "-x", "-c", command, m.MountRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xfs_quota %q failed: %w: %s", command, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shellToken(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

func findXFSQuota() (string, error) {
	if path, err := exec.LookPath("xfs_quota"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/usr/sbin/xfs_quota", "/sbin/xfs_quota", "/usr/bin/xfs_quota"} {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("quota requested but xfs_quota is unavailable")
}
