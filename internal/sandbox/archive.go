package sandbox

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteTree writes the tree at root to w as a tar archive, every entry owned
// by uid:gid with no user or group names. Symlinks are archived as
// symlinks, never followed: a link pointing outside the tree resolves inside
// the sandbox it is unpacked in, where the only thing to reach is the
// sandbox. Sockets, devices and pipes are skipped.
//
// Moved here from internal/sandbox/docker/archive.go in ticket 7, unchanged
// in behaviour, so the gate's staging (internal/gate) and later backends
// stream a prepared tree with the same writer the docker backend uses.
func WriteTree(w io.Writer, root string, uid, gid int) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return writeEntry(tw, path, filepath.ToSlash(rel), d, uid, gid)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func writeEntry(tw *tar.Writer, path, name string, d fs.DirEntry, uid, gid int) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}
	if !info.Mode().IsRegular() && !info.IsDir() && link == "" {
		// Sockets, devices and pipes cannot be part of a working tree.
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("header for %s: %w", name, err)
	}
	hdr.Name = name
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.Uid, hdr.Gid = uid, gid
	hdr.Uname, hdr.Gname = "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing header for %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}
