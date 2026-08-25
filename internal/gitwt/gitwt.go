// Package gitwt wraps `git worktree` — the isolation primitive that lets
// several agents work on the same repository at the same time without
// stepping on each other's uncommitted changes: each agent gets its own
// working directory checked out to its own branch, sharing the same .git
// object store (cheap — no full clone).
package gitwt

import (
	"fmt"
	"os/exec"
	"strings"
)

// RepoRoot resolves dir to the top-level directory of the git repository it
// belongs to. Returns an error (not a git repo) if dir isn't inside one.
func RepoRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("%q não é um repositório git (ou o git não está instalado): %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// BranchExists reports whether branch already exists in the repo at
// repoRoot (local branches only).
func BranchExists(repoRoot, branch string) bool {
	return exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// AddWorktree creates a new worktree at worktreePath. If branch already
// exists, the worktree checks it out as-is (picking up where a previous
// agent using the same branch left off); otherwise a new branch is created
// from the repo's current HEAD.
func AddWorktree(repoRoot, worktreePath, branch string) error {
	var args []string
	if BranchExists(repoRoot, branch) {
		args = []string{"-C", repoRoot, "worktree", "add", worktreePath, branch}
	} else {
		args = []string{"-C", repoRoot, "worktree", "add", "-b", branch, worktreePath}
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveWorktree removes a worktree. force discards uncommitted changes in
// it — without force, git refuses to remove a worktree with a dirty
// working tree (the safe default: a caller has to opt into losing work).
func RemoveWorktree(repoRoot, worktreePath string, force bool) error {
	args := []string{"-C", repoRoot, "worktree", "remove", worktreePath}
	if force {
		args = append(args, "--force")
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeleteBranch force-deletes a local branch. Only ever called when the
// caller explicitly asked to discard a worktree's branch too — never
// implicit, since it can drop commits with no worktree left to recover
// them from.
func DeleteBranch(repoRoot, branch string) error {
	out, err := exec.Command("git", "-C", repoRoot, "branch", "-D", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git branch -D: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
