// git plumbing the base comparison needs. Keeping it here leaves
// internal/adrguard a pure filesystem package that any test can exercise
// without a repository.
package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path"
	"strings"
)

// rev resolves a ref to the commit it names, so a ref that is absent from this
// checkout fails with git's own words instead of reading as an empty tree.
func rev(args ...string) (string, error) {
	out, err := git(args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// git runs a git command and returns its stdout, carrying stderr into the error
// so a failure says which command and why.
func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// treePaths lists the files under dirs at ref, repo-relative and slash
// separated, the same shape a scan of the working tree reports. A ref or a
// directory without records yields no paths, which is a side with nothing to
// collide, not an error.
func treePaths(ref string, dirs []string) ([]string, error) {
	seen := map[string]bool{}
	var paths []string
	for _, dir := range dirs {
		out, err := git("ls-tree", "-r", "--name-only", ref, "--", dir)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(out, "\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	return paths, nil
}

// gitReader reads one path out of ref. It is the reader a claim needs when the
// file lives only on the base side: its title is what tells a reader whether
// the shared number names two decisions or one file that moved.
func gitReader(ref string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		out, err := git("show", ref+":"+path.Clean(p))
		return []byte(out), err
	}
}
