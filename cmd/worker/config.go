package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/austmsk/conclave/internal/activity"
	"github.com/austmsk/conclave/internal/agent"
	"github.com/austmsk/conclave/internal/deployfile"
	"github.com/austmsk/conclave/internal/gate"
	"github.com/austmsk/conclave/internal/github"
	"github.com/austmsk/conclave/internal/llm"
	"github.com/austmsk/conclave/internal/sandbox"
)

// paths names the deploy files the worker loads. Flags default them to
// deploy/*.yaml, whose placeholders refuse to load; the owner points them
// at deploy/local/.
type paths struct {
	Router, Repos, Toolchains, Sandboxes, Gates, GitHub string
}

// deploy is every deploy file, validated, with the SHA-256 of the bytes
// each was loaded from. Every plan body records the digests.
type deploy struct {
	Router     llm.Config
	Access     map[string]llm.RepoAccess
	Toolchains map[string]agent.Toolchain
	Sandboxes  map[string]sandbox.RepoSandbox
	Gates      gate.Config
	GitHub     github.Config
	Digests    map[string]string
}

// loadDeploy loads and validates every deploy file, then checks the four
// per-repository files name the same repositories. Any failure stops
// startup with the file's path in the message.
func loadDeploy(p paths) (deploy, error) {
	var d deploy
	d.Digests = map[string]string{}
	loads := []struct {
		path string
		load func(io.Reader) error
	}{
		{p.Router, func(r io.Reader) (err error) { d.Router, err = llm.LoadConfig(r); return err }},
		{p.Repos, func(r io.Reader) (err error) { d.Access, err = llm.LoadRepoAccess(r); return err }},
		{p.Toolchains, func(r io.Reader) (err error) { d.Toolchains, err = agent.LoadToolchains(r); return err }},
		{p.Sandboxes, func(r io.Reader) (err error) { d.Sandboxes, err = sandbox.LoadConfig(r); return err }},
		{p.Gates, func(r io.Reader) (err error) { d.Gates, err = gate.LoadConfig(r); return err }},
		{p.GitHub, func(r io.Reader) (err error) { d.GitHub, err = github.LoadConfig(r); return err }},
	}
	for _, l := range loads {
		digest, err := deployfile.Load(l.path, l.load)
		if err != nil {
			return deploy{}, err
		}
		d.Digests[filepath.Base(l.path)] = "sha256:" + digest
	}
	if err := sameRepositories(map[string][]string{
		filepath.Base(p.Repos):      slices.Collect(maps.Keys(d.Access)),
		filepath.Base(p.Toolchains): slices.Collect(maps.Keys(d.Toolchains)),
		filepath.Base(p.Sandboxes):  slices.Collect(maps.Keys(d.Sandboxes)),
		filepath.Base(p.Gates):      slices.Collect(maps.Keys(d.Gates.Repositories)),
	}); err != nil {
		return deploy{}, err
	}
	return d, nil
}

// repos joins the four per-repository files into the activities' view,
// keyed by full name. loadDeploy has checked they name the same set.
func (d deploy) repos() map[string]activity.Repo {
	out := make(map[string]activity.Repo, len(d.Access))
	for name, access := range d.Access {
		out[name] = activity.Repo{
			Access: access, Sandbox: d.Sandboxes[name], Toolchain: d.Toolchains[name], Gates: d.Gates.Repositories[name],
		}
	}
	return out
}

// sameRepositories checks that every file names exactly the same
// repositories, and names each one that is missing from some file.
func sameRepositories(byFile map[string][]string) error {
	all := map[string]bool{}
	for _, names := range byFile {
		for _, n := range names {
			all[n] = true
		}
	}
	var problems []string
	for _, file := range slices.Sorted(maps.Keys(byFile)) {
		have := byFile[file]
		for _, n := range slices.Sorted(maps.Keys(all)) {
			if !slices.Contains(have, n) {
				problems = append(problems, fmt.Sprintf("%s does not configure %s", file, n))
			}
		}
	}
	if len(problems) > 0 {
		return errors.New("deploy files disagree on the repositories: " + strings.Join(problems, "; "))
	}
	return nil
}
