package secrets

import (
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/sourceref"
)

// Which discoboxes an approval rule is for is a list of repositories, compared
// exactly and by the server (ADR 26-10-12-330 §2): no model reads it. A rule
// with none is for every discobox in the project.
//
// A repository's identity is its host and path — github.com/org/repo — as
// sourceref.NormalizeRemote reduces a remote to: no scheme or user, the host
// lower-cased, no trailing slash or ".git", so the HTTPS and SSH spellings of
// one repository are one. Both sides of the comparison are made here: what a
// rule is given (ruleRepository) and what a discobox has (repositoryIdentity).

const (
	// maxRuleRepositories is the most repositories one rule names. A rule for
	// more than this is a rule for every discobox, or several rules.
	maxRuleRepositories = 32
	// maxRuleRepository is the longest one may be.
	maxRuleRepository = 512
)

// repositoryIdentity is the repository a discobox is of, for a rule to be
// compared with: its primary source's URL, else the remote a local source's
// checkout tracks, when the client that created the discobox said one. Its
// other sources never count. A local source with no remote has none, and only
// a rule for every discobox answers it: a path on somebody's machine names no
// repository.
func repositoryIdentity(source *model.GitSource) string {
	if source == nil {
		return ""
	}
	return sourceref.NormalizeRemote(source.RepositoryURL())
}

// ruleRepository is one repository as a rule stores it, from what a person
// gave: a URL, git's scp-like form, or host/path. A bare org/repo names no
// host, and is refused rather than guessed at: the same path on another host
// is another repository.
func ruleRepository(given string) (string, error) {
	given = strings.TrimSpace(given)
	if given == "" {
		return "", fmt.Errorf("a rule's repository is a URL or host/path, such as github.com/org/repo, and not empty")
	}
	if len(given) > maxRuleRepository {
		return "", fmt.Errorf("a rule's repository is at most %d bytes", maxRuleRepository)
	}
	if strings.Contains(given, "://") {
		if identity := sourceref.NormalizeRemote(given); identity != "" {
			return identity, nil
		}
		return "", fmt.Errorf("%q is not a repository's URL", given)
	}
	// host/path, which is also how an identity is stored: read before the
	// scp-like form, so a stored host:port/path reads back as itself.
	host, path, _ := strings.Cut(given, "/")
	if name, port, ported := strings.Cut(host, ":"); !strings.Contains(host, "@") && (!ported || isPort(port)) {
		if !strings.Contains(name, ".") {
			if strings.Trim(path, "/") == "" {
				return "", fmt.Errorf("%q names no repository: say its host and path, as in github.com/org/repo", given)
			}
			return "", fmt.Errorf("%q names no host: say which, as in github.com/%s", given, strings.Trim(given, "/"))
		}
		if identity := sourceref.NormalizeRemote("https://" + given); identity != "" {
			return identity, nil
		}
		return "", fmt.Errorf("%q names no repository: say its host and path, as in github.com/org/repo", given)
	}
	if identity := sourceref.NormalizeRemote(given); identity != "" {
		return identity, nil
	}
	return "", fmt.Errorf("%q is not a repository: give a URL, or host/path as in github.com/org/repo", given)
}

func isPort(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ruleRepositories holds the repositories a rule was given to what a rule may
// name, and returns them as stored: each an identity, said once, in the order
// given. Two spellings of one repository are one entry said twice, which is
// refused so that what a person reads back is what they wrote.
func ruleRepositories(given []string) ([]string, error) {
	if len(given) > maxRuleRepositories {
		return nil, fmt.Errorf("a rule names at most %d repositories; one for more is a rule for every discobox, or several rules", maxRuleRepositories)
	}
	out := make([]string, 0, len(given))
	for _, one := range given {
		identity, err := ruleRepository(one)
		if err != nil {
			return nil, err
		}
		if slices.Contains(out, identity) {
			return nil, fmt.Errorf("a rule names each repository once, and %s is named twice", identity)
		}
		out = append(out, identity)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ruleRepositoryProblem says why a rule is not for a discobox of the
// repository given, or nothing when it is: a rule with no repositories is for
// every discobox, and one with some is for a discobox of one of them.
func ruleRepositoryProblem(rule *model.ApprovalRule, repository string) string {
	switch {
	case len(rule.Repositories) == 0 || repository != "" && slices.Contains(rule.Repositories, repository):
		return ""
	case repository == "":
		return fmt.Sprintf("the rule is for discoboxes of %s, and this discobox's source has no repository to compare: only a rule for every discobox answers it",
			strings.Join(rule.Repositories, ", "))
	}
	return fmt.Sprintf("the rule is for discoboxes of %s, and this discobox is of %s", strings.Join(rule.Repositories, ", "), repository)
}
