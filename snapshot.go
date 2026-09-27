package stellarjay

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SnapshotInfo struct {
	Path      string    `json:"path"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
	Nodes     int       `json:"nodes"`
}

type snapshotManifest struct {
	Format    int       `json:"format"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"created_at"`
	Nodes     int       `json:"nodes"`
	Key       string    `json:"key"`
}

// Snapshot writes a consistent gzip-compressed tar archive containing all
// encrypted nodes and refs. Encryption keys are deliberately excluded so a
// copied backup is not sufficient to decrypt the data.
func (s *Store) Snapshot(dest string) (SnapshotInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot(dest, s.now().UTC())
}

// CreateSnapshot creates a timestamped snapshot in dir. The filename and
// manifest timestamp are derived from the same store clock reading.
func (s *Store) CreateSnapshot(dir string) (SnapshotInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	created := s.now().UTC()
	name := fmt.Sprintf("stellarjay-%s.tar.gz", created.Format("20060102T150405.000000000Z"))
	info, err := s.snapshot(filepath.Join(dir, name), created)
	if err != nil {
		return SnapshotInfo{}, err
	}
	info.Path = name
	return info, nil
}

// SnapshotSizeEstimate returns a conservative estimate of the temporary free
// space needed to create a gzip snapshot. This intentionally scans only files
// that are included in snapshots and is used only by the admin endpoint.
func (s *Store) SnapshotSizeEstimate() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total uint64 = 1 << 20
	for _, subtree := range []string{"objects/nodes", "refs"} {
		base := filepath.Join(s.dir, filepath.FromSlash(subtree))
		err := filepath.Walk(base, func(path string, fileInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if fileInfo.IsDir() {
				return nil
			}
			if !fileInfo.Mode().IsRegular() {
				return fmt.Errorf("refusing to snapshot non-regular file %s", path)
			}
			total += uint64(fileInfo.Size()) + 1024
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (s *Store) snapshot(dest string, created time.Time) (SnapshotInfo, error) {

	root, err := s.currentRoot()
	if err != nil {
		return SnapshotInfo{}, err
	}
	nodes, err := s.nodesFromRoot(root)
	if err != nil {
		return SnapshotInfo{}, err
	}
	info := SnapshotInfo{Path: dest, Root: root, CreatedAt: created, Nodes: len(nodes)}
	manifest := snapshotManifest{
		Format: 1, Root: root, CreatedAt: created, Nodes: len(nodes),
		Key: "not included; restore with the original STELLARJAY_DATA_KEY",
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return SnapshotInfo{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".stellarjay-snapshot-*.tar.gz")
	if err != nil {
		return SnapshotInfo{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return SnapshotInfo{}, err
	}

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	closeWithError := func(base error) error {
		if err := tw.Close(); base == nil {
			base = err
		}
		if err := gz.Close(); base == nil {
			base = err
		}
		if err := tmp.Sync(); base == nil {
			base = err
		}
		if err := tmp.Close(); base == nil {
			base = err
		}
		return base
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err == nil {
		manifestBytes = append(manifestBytes, '\n')
		err = writeTarBytes(tw, "manifest.json", manifestBytes, created)
	}
	for _, subtree := range []string{"objects/nodes", "refs"} {
		if err != nil {
			break
		}
		base := filepath.Join(s.dir, filepath.FromSlash(subtree))
		err = filepath.Walk(base, func(path string, fileInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if fileInfo.IsDir() {
				return nil
			}
			if !fileInfo.Mode().IsRegular() {
				return fmt.Errorf("refusing to snapshot non-regular file %s", path)
			}
			rel, err := filepath.Rel(s.dir, path)
			if err != nil {
				return err
			}
			return writeTarFile(tw, path, filepath.ToSlash(rel), fileInfo)
		})
	}
	if err = closeWithError(err); err != nil {
		return SnapshotInfo{}, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return SnapshotInfo{}, err
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return SnapshotInfo{}, err
	}
	return info, nil
}

func writeTarBytes(tw *tar.Writer, name string, data []byte, modified time.Time) error {
	header := &tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: modified,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeTarFile(tw *tar.Writer, path, name string, info os.FileInfo) error {
	if strings.HasPrefix(name, "../") || filepath.IsAbs(name) {
		return fmt.Errorf("refusing unsafe snapshot path %q", name)
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.Mode = 0o600
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(tw, f)
	return err
}
