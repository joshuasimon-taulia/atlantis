package events

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/models"
	"github.com/runatlantis/atlantis/server/events/vcs"
	"github.com/runatlantis/atlantis/server/logging"
)

// GithubAppWorkingDir implements WorkingDir.
// It acts as a proxy to an instance of WorkingDir that refreshes the app's token
// before every clone, given Github App tokens expire quickly
type GithubAppWorkingDir struct {
	WorkingDir
	Credentials    vcs.GithubCredentials
	GithubHostname string
}

type GithubAppFileWorkspace struct {
	FileWorkspace
}

// Clone writes a fresh token for Github App authentication
func (g *GithubAppWorkingDir) Clone(log logging.SimpleLogging, headRepo models.Repo, p models.PullRequest, workspace string) (string, bool, error) {

	log.Info("Refreshing git tokens for Github App")

	token, err := g.Credentials.GetToken()
	if err != nil {
		return "", false, errors.Wrap(err, "getting github token")
	}

	home, err := homedir.Dir()
	if err != nil {
		return "", false, errors.Wrap(err, "getting home dir to write ~/.git-credentials file")
	}

	// https://developer.github.com/apps/building-github-apps/authenticating-with-github-apps/#http-based-git-access-by-an-installation
	if err := WriteGitCreds("x-access-token", token, g.GithubHostname, home, log, true); err != nil {
		return "", false, err
	}

	baseRepo := &p.BaseRepo

	// Realistically, this is a super brittle way of supporting clones using gh app installation tokens
	// This URL should be built during Repo creation and the struct should be immutable going forward.
	// Doing this requires a larger refactor however, and can probably be coupled with supporting > 1 installation
	authURL := fmt.Sprintf("://x-access-token:%s", token)
	baseRepo.CloneURL = strings.Replace(baseRepo.CloneURL, "://:", authURL, 1)
	baseRepo.SanitizedCloneURL = strings.Replace(baseRepo.SanitizedCloneURL, "://:", "://x-access-token:", 1)
	headRepo.CloneURL = strings.Replace(headRepo.CloneURL, "://:", authURL, 1)
	headRepo.SanitizedCloneURL = strings.Replace(baseRepo.SanitizedCloneURL, "://:", "://x-access-token:", 1)

	return g.WorkingDir.Clone(log, headRepo, p, workspace)
}

func (g *GithubAppFileWorkspace) forceClone(log logging.SimpleLogging,
	cloneDir string,
	headRepo models.Repo,
	p models.PullRequest) error {

	err := os.RemoveAll(cloneDir)
	if err != nil {
		return errors.Wrapf(err, "deleting dir %q before cloning", cloneDir)
	}

	// Create the directory and parents if necessary.
	log.Info("creating dir %q", cloneDir)
	if err := os.MkdirAll(cloneDir, 0700); err != nil {
		return errors.Wrap(err, "creating new workspace")
	}

	// During testing, we mock some of this out.
	headCloneURL := headRepo.CloneURL
	if g.FileWorkspace.TestingOverrideHeadCloneURL != "" {
		headCloneURL = g.FileWorkspace.TestingOverrideHeadCloneURL
	}
	baseCloneURL := p.BaseRepo.CloneURL
	if g.FileWorkspace.TestingOverrideBaseCloneURL != "" {
		baseCloneURL = g.FileWorkspace.TestingOverrideBaseCloneURL
	}

	var cmds [][]string
	if g.FileWorkspace.CheckoutMerge {
		// NOTE: We can't do a shallow clone when we're merging because we'll
		// get merge conflicts if our clone doesn't have the commits that the
		// branch we're merging branched off at.
		// See https://groups.google.com/forum/#!topic/git-users/v3MkuuiDJ98.
		cmds = [][]string{
			{
				"git", "clone", "--branch", p.BaseBranch, "--single-branch", baseCloneURL, cloneDir,
			},
			{
				"git", "remote", "add", "head", headCloneURL,
			},
			{
				"git", "fetch", "head", fmt.Sprintf("pull/%s/head:", p.Num),
			},
			// We use --no-ff because we always want there to be a merge commit.
			// This way, our branch will look the same regardless if the merge
			// could be fast forwarded. This is useful later when we run
			// git rev-parse HEAD^2 to get the head commit because it will
			// always succeed whereas without --no-ff, if the merge was fast
			// forwarded then git rev-parse HEAD^2 would fail.
			{
				"git", "merge", "-q", "--no-ff", "-m", "atlantis-merge", "FETCH_HEAD",
			},
		}
	} else {
		cmds = [][]string{
			{
				"git", "clone", "--branch", p.HeadBranch, "--depth=1", "--single-branch", headCloneURL, cloneDir,
			},
		}
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...) // nolint: gosec
		cmd.Dir = cloneDir
		// The git merge command requires these env vars are set.
		cmd.Env = append(os.Environ(), []string{
			"EMAIL=atlantis@runatlantis.io",
			"GIT_AUTHOR_NAME=atlantis",
			"GIT_COMMITTER_NAME=atlantis",
		}...)

		cmdStr := g.FileWorkspace.sanitizeGitCredentials(strings.Join(cmd.Args, " "), p.BaseRepo, headRepo)
		output, err := cmd.CombinedOutput()
		sanitizedOutput := g.FileWorkspace.sanitizeGitCredentials(string(output), p.BaseRepo, headRepo)
		if err != nil {
			sanitizedErrMsg := g.FileWorkspace.sanitizeGitCredentials(err.Error(), p.BaseRepo, headRepo)
			return fmt.Errorf("running %s: %s: %s", cmdStr, sanitizedOutput, sanitizedErrMsg)
		}
		log.Debug("ran: %s. Output: %s", cmdStr, strings.TrimSuffix(sanitizedOutput, "\n"))
	}
	return nil
}
