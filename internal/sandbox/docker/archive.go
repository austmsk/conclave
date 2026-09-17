package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// copyTreeIn streams the prepared tree into the workspace as a tar archive
// unpacked by tar inside the container, running as the sandbox user. The
// Engine's own archive endpoint refuses read-only containers, and going
// through exec also means the files are owned by the user that will edit
// them. Symlinks are archived as symlinks, never followed: a link pointing
// outside the tree resolves inside the container, where the only thing to
// reach is the container.
func (s *Sandbox) copyTreeIn(ctx context.Context, treePath string) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := writeTree(pw, treePath, s.uid, s.gid)
		_ = pw.CloseWithError(err)
		errc <- err
	}()
	s.mu.Lock()
	res, err := s.run(ctx, execSpec{
		cmd:     []string{"tar", "-x", "-f", "-", "-C", workspaceDir},
		workDir: workspaceDir,
		stdin:   pr,
	})
	s.mu.Unlock()
	_ = pr.Close()
	if werr := <-errc; werr != nil {
		return fmt.Errorf("archiving %s: %w", treePath, werr)
	}
	if err != nil {
		return fmt.Errorf("unpacking archive: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("unpacking archive: tar exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

func writeTree(w io.Writer, root string, uid, gid int) error {
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
