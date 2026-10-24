package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/austmsk/conclave/internal/sandbox"
)

// copyTreeIn streams the prepared tree into the workspace as a tar archive
// unpacked by tar inside the container, running as the sandbox user. The
// Engine's own archive endpoint refuses read-only containers, and going
// through exec also means the files are owned by the user that will edit
// them. sandbox.WriteTree archives symlinks as symlinks, never followed.
func (s *Sandbox) copyTreeIn(ctx context.Context, treePath string) error {
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := sandbox.WriteTree(pw, treePath, s.uid, s.gid)
		_ = pw.CloseWithError(err)
		errc <- err
	}()
	s.ops.Lock()
	res, err := s.run(ctx, execSpec{
		cmd:     []string{"tar", "-x", "-f", "-", "-C", workspaceDir},
		workDir: workspaceDir,
		stdin:   pr,
	})
	s.ops.Unlock()
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
