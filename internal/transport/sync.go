package transport

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blackstorm/bsdyak/internal/protocol"
	"github.com/klauspost/compress/zstd"
)

type ignoreMatcher struct {
	patterns []string
}

func loadIgnoreMatcher(root string) *ignoreMatcher {
	m := &ignoreMatcher{}
	for _, name := range []string{".gitignore", ".yakignore"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
				continue
			}
			m.patterns = append(m.patterns, line)
		}
	}
	return m
}

func (m *ignoreMatcher) match(relSlash string, isDir bool) bool {
	if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") ||
		relSlash == ".yak" || strings.HasPrefix(relSlash, ".yak/") {
		return true
	}
	base := path.Base(relSlash)
	for _, p := range m.patterns {
		dirOnly := strings.HasSuffix(p, "/")
		clean := strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/")
		if clean == "" {
			continue
		}
		if dirOnly && !isDir {
			// Also match files inside an ignored directory prefix.
			if relSlash == clean || strings.HasPrefix(relSlash, clean+"/") {
				return true
			}
			continue
		}
		if strings.Contains(clean, "/") {
			if ok, _ := path.Match(clean, relSlash); ok || strings.HasPrefix(relSlash, clean+"/") {
				return true
			}
		} else {
			if ok, _ := path.Match(clean, base); ok || relSlash == clean || strings.HasPrefix(relSlash, clean+"/") {
				return true
			}
		}
	}
	return false
}

// BuildSyncManifest scans rootDir respecting .gitignore and .yakignore.
func BuildSyncManifest(rootDir string) (map[string]protocol.FileMeta, error) {
	matcher := loadIgnoreMatcher(rootDir)
	files := make(map[string]protocol.FileMeta)

	err := filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == rootDir {
			return nil
		}
		rel, err := filepath.Rel(rootDir, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if matcher.match(relSlash, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		sum, err := sha256File(p)
		if err != nil {
			return err
		}
		files[relSlash] = protocol.FileMeta{
			Size:   info.Size(),
			Mode:   uint32(info.Mode().Perm()),
			SHA256: sum,
		}
		return nil
	})
	return files, err
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PackZstdTar writes the specified relative files from rootDir as a zstd-compressed tar stream.
func PackZstdTar(rootDir string, relPaths []string, out io.Writer) error {
	zw, err := zstd.NewWriter(out)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)

	sorted := append([]string(nil), relPaths...)
	sort.Strings(sorted)

	for _, rel := range sorted {
		clean := path.Clean(filepath.ToSlash(rel))
		if strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
			_ = tw.Close()
			_ = zw.Close()
			return fmt.Errorf("invalid relative path %q", rel)
		}
		full := filepath.Join(rootDir, filepath.FromSlash(clean))
		info, err := os.Stat(full)
		if err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return err
		}
		hdr := &tar.Header{
			Name: clean,
			Mode: int64(info.Mode().Perm()),
			Size: info.Size(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return err
		}
		f, err := os.Open(full)
		if err != nil {
			_ = tw.Close()
			_ = zw.Close()
			return err
		}
		_, cErr := io.Copy(tw, f)
		_ = f.Close()
		if cErr != nil {
			_ = tw.Close()
			_ = zw.Close()
			return cErr
		}
	}

	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// UnpackZstdTar safely extracts a zstd-compressed tar archive into dstDir, preventing path traversal.
func UnpackZstdTar(r io.Reader, dstDir string) ([]string, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	absDst, err := filepath.Abs(dstDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absDst, 0o755); err != nil {
		return nil, err
	}

	tr := tar.NewReader(zr)
	var extracted []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		clean := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." || path.IsAbs(clean) {
			return nil, fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		target := filepath.Join(absDst, filepath.FromSlash(clean))
		rel, err := filepath.Rel(absDst, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("archive path escapes destination: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			mode := os.FileMode(hdr.Mode & 0o777)
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return nil, err
			}
			_, cErr := io.Copy(f, tr)
			_ = f.Close()
			if cErr != nil {
				return nil, cErr
			}
			extracted = append(extracted, clean)
		}
	}
	return extracted, nil
}
