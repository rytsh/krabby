package transfer

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

const maxManifestBytes = 64 << 20

func writeArchive(ctx context.Context, out io.Writer, dir string, streams map[string]string, m *Manifest) error {
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	defer gz.Close()
	defer tw.Close()
	add := func(src, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(src)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(src)
			if err != nil {
				return err
			}
			if !safeLink(name, link) {
				return fmt.Errorf("cannot transfer escaping symlink %s -> %s", name, link)
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type: %s", src)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = name, 0, 0, "", ""
		hdr.Mode &= 0o777
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		h := sha256.New()
		switch hdr.Typeflag {
		case tar.TypeDir:
			_, _ = h.Write([]byte("dir"))
		case tar.TypeSymlink:
			_, _ = h.Write([]byte("link\x00" + link))
		default:
			_, _ = h.Write([]byte("file\x00"))
			f, err := os.Open(src)
			if err != nil {
				return err
			}
			_, err = io.Copy(io.MultiWriter(tw, h), &contextReader{ctx: ctx, r: f})
			cerr := f.Close()
			if err != nil {
				return err
			}
			if cerr != nil {
				return cerr
			}
		}
		m.Files[name] = hex.EncodeToString(h.Sum(nil))
		return nil
	}
	for _, name := range databases {
		if stream, ok := streams[name]; ok {
			if err := add(stream, "databases/"+name); err != nil {
				return err
			}
		}
	}
	for _, top := range fileRoots {
		root := filepath.Join(dir, top)
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			return add(p, "files/"+filepath.ToSlash(rel))
		}); err != nil {
			return err
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) > maxManifestBytes {
		return fmt.Errorf("transfer manifest exceeds %d bytes", maxManifestBytes)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(data))}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func safeName(name string) bool {
	if !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return false
	}
	if name == "manifest.json" {
		return true
	}
	if n, ok := strings.CutPrefix(name, "databases/"); ok {
		return slices.Contains(databases, n)
	}
	if n, ok := strings.CutPrefix(name, "files/"); ok {
		top, _, _ := strings.Cut(n, "/")
		return slices.Contains(fileRoots, top)
	}
	return false
}

func safeLink(name, target string) bool {
	if target == "" || path.IsAbs(target) || strings.Contains(target, "\\") {
		return false
	}
	resolved := path.Join(path.Dir(name), target)
	return strings.HasPrefix(name, "files/") && strings.HasPrefix(resolved, "files/") && safeName(resolved)
}

// readArchive extracts into a private directory. Symlinks are created last so
// no later member can write through a link planted by an earlier member.
func readArchive(ctx context.Context, input io.Reader, dir string) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(&contextReader{ctx: ctx, r: input})
	if err != nil {
		return m, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return m, err
	}
	defer root.Close()
	seen := map[string]string{}
	links := map[string]string{}
	manifestSeen := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, err
		}
		name := hdr.Name
		if !safeName(name) {
			return m, fmt.Errorf("invalid archive path %q", name)
		}
		if _, exists := seen[name]; exists {
			return m, fmt.Errorf("duplicate archive entry %q", name)
		}
		if name == "manifest.json" {
			if manifestSeen || hdr.Typeflag != tar.TypeReg || hdr.Size > maxManifestBytes {
				return m, fmt.Errorf("invalid manifest entry")
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return m, err
			}
			if err := json.Unmarshal(data, &m); err != nil {
				return m, err
			}
			manifestSeen = true
			continue
		}
		if strings.HasPrefix(name, "databases/") && hdr.Typeflag != tar.TypeReg {
			return m, fmt.Errorf("invalid database entry")
		}
		if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			return m, err
		}
		h := sha256.New()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return m, err
			}
			_, _ = h.Write([]byte("dir"))
		case tar.TypeSymlink:
			if !safeLink(name, hdr.Linkname) {
				return m, fmt.Errorf("invalid symlink %q", name)
			}
			links[name] = hdr.Linkname
			_, _ = h.Write([]byte("link\x00" + hdr.Linkname))
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(hdr.Mode)&0o700|0o600)
			if err != nil {
				return m, err
			}
			_, _ = h.Write([]byte("file\x00"))
			_, err = io.Copy(io.MultiWriter(f, h), tr)
			if err == nil {
				err = f.Sync()
			}
			cerr := f.Close()
			if err != nil {
				return m, err
			}
			if cerr != nil {
				return m, cerr
			}
		default:
			return m, fmt.Errorf("unsupported archive member %q", name)
		}
		seen[name] = hex.EncodeToString(h.Sum(nil))
	}
	// Consume gzip's trailer: tar EOF alone does not validate the gzip checksum.
	if n, err := io.Copy(io.Discard, gz); err != nil {
		return m, err
	} else if n != 0 {
		return m, fmt.Errorf("unexpected trailing archive data")
	}
	if !manifestSeen || !reflect.DeepEqual(m.Files, seen) {
		return m, fmt.Errorf("archive checksums or inventory do not match manifest")
	}
	for name, target := range links {
		if err := root.Symlink(target, name); err != nil {
			return m, err
		}
	}
	return m, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
