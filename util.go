/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"golang.org/x/mod/modfile"
	"golang.org/x/net/html"
)

const (
	vendorConf = "vendor.conf"
	modulesTxt = "vendor/modules.txt"
	goMod      = "go.mod"
)

var (
	errUnknownFormat = errors.New("unknown file format")
)

func loadRelease(path string) (*release, error) {
	var r release
	if _, err := toml.DecodeFile(path, &r); err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("please specify the release file as the first argument")
		}
		return nil, err
	}
	return &r, nil
}

func parseTag(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".toml")
}

func parseDependencies(commit string) ([]dependency, error) {
	rd, err := fileFromRev(commit, vendorConf)
	if err == nil {
		return parseVendorConfDependencies(rd)
	}
	rd, err = fileFromRev(commit, modulesTxt)
	if err == nil {
		return parseModulesTxtDependencies(rd)
	}
	rd, err = fileFromRev(commit, goMod)
	if err == nil {
		return parseGoModDependencies(rd)
	}
	return nil, errors.Errorf("finding dependency file failed: %v", err)
}

func parseModulesTxtDependencies(r io.Reader) ([]dependency, error) {
	var dependencies []dependency
	s := bufio.NewScanner(r)
	for s.Scan() {
		ln := strings.TrimSpace(s.Text())
		if ln == "" {
			continue
		}
		parts := strings.Fields(ln)
		if parts[0] != "#" {
			continue
		}

		// See https://golang.org/ref/mod#go-mod-file-replace for
		// syntax on replace directives
		var commitOrVersionPart string
		if len(parts) == 3 {
			commitOrVersionPart = parts[2]
		} else if len(parts) == 5 && parts[2] == "=>" {
			// replace directive in go.mod without old version
			// no need to care since it will has corresponding one with old version
			continue
		} else if len(parts) == 6 && parts[3] == "=>" {
			commitOrVersionPart = parts[5]
		} else if (len(parts) == 4 && parts[2] == "=>") || (len(parts) == 5 && parts[3] == "=>") {
			// Ignore replace directive which uses filepath
			continue
		} else {
			return nil, errors.Wrapf(errUnknownFormat, "%s", ln)
		}
		commitOrVersion, isSha := getCommitOrVersion(commitOrVersionPart)
		if commitOrVersion == "" {
			return nil, errors.Wrapf(errUnknownFormat, "poorly formatted version in replace section %s", parts[2])
		}

		dependencies = append(dependencies, formatDependency(parts[1], commitOrVersion, isSha))
	}
	return dependencies, nil
}

func parseGoModDependencies(r io.Reader) ([]dependency, error) {
	var err error

	contents, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	goMod, err := modfile.ParseLax("go.mod", contents, nil)
	if err != nil {
		return nil, err
	}

	depMap := make(map[string]*dependency)
	replaceMap := make(map[string]*dependency)

	for _, require := range goMod.Require {
		commitOrVersion, isSha := getCommitOrVersion(require.Mod.Version)
		if commitOrVersion == "" {
			return nil, errors.Wrapf(errUnknownFormat, "poorly formatted version in require section %s", require.Mod)
		}

		dep := formatDependency(require.Mod.Path, commitOrVersion, isSha)
		depMap[dep.Name] = &dep
	}

	for _, replace := range goMod.Replace {
		if strings.HasPrefix(replace.New.Path, "./") {
			continue
		}

		commitOrVersion, isSha := getCommitOrVersion(replace.New.Version)
		if commitOrVersion == "" {
			return nil, errors.Wrapf(errUnknownFormat, "poorly formatted version in replace section %s", replace.New)
		}

		dep := formatDependency(replace.New.Path, commitOrVersion, isSha)
		replaceMap[dep.Name] = &dep
	}

	for depName, dep := range replaceMap {
		if oldDep, ok := depMap[depName]; ok {
			oldDep.Ref = dep.Ref
			oldDep.Sha = dep.Sha
			oldDep.GitURL = dep.GitURL
		} else {
			logrus.Debugf("dependency %s found in replace section, but doesn't exist in requires section. Skipping", depName)
			continue
		}
	}
	var deps []dependency
	for _, dep := range depMap {
		deps = append(deps, *dep)
	}

	return deps, nil
}

func sanitizeLine(line, commentDelim string) string {
	ln := strings.TrimSpace(line)
	if ln == "" {
		return ""
	}
	cidx := strings.Index(ln, commentDelim)
	// whole line is commented
	if cidx == 0 {
		return ""
	}
	if cidx > 0 {
		ln = ln[:cidx]
	}

	return strings.TrimSpace(ln)
}

// getCommitOrVersion parses the commit or version from go modules
// and returns the commit sha or ref and whether the result is a git sha
func getCommitOrVersion(cov string) (string, bool) {
	// parse the commit or version. It'll either be of the form
	// v0.0.0 or v0.0.0-date-commitID. Split by '-' to check
	dashFields := strings.FieldsFunc(cov, func(c rune) bool { return c == '-' })
	fieldsLen := len(dashFields)

	if fieldsLen > 3 {
		// empty string signifies error to caller
		return "", false
	}

	var isSha bool

	// if dashFields has one or two fields, it is likely a version (possibly with a -rc1).
	// Thus, it should be used as is.
	// the only case we meddle is when there are three fields, so we can strip the commitID
	if len(dashFields) == 3 {
		// If there are three fields, use the last (the commit)
		// as often the version found in the first field is just a placeholder
		cov = dashFields[2]
		isSha = true
	}

	// despite it being idiomatic to go modules, the +incompatible is a bit
	// unsightly in release notes. Let's cut it out of the version if it
	// exists
	if incpIdx := strings.Index(cov, "+incompatible"); incpIdx > 0 {
		return cov[:incpIdx], isSha
	}
	return cov, isSha
}

func formatDependency(name, commitOrVersion string, isSha bool) dependency {
	var sha string
	if isSha {
		sha = commitOrVersion
	}
	return dependency{
		Name:   name,
		Ref:    commitOrVersion,
		Sha:    sha,
		GitURL: getGitURL(name),
	}
}

// getGitURL gets known git clone URLs from names
// If an empty string is returned, then this must
// be checked using `?go-get=1`
func getGitURL(name string) string {
	if idx := strings.Index(name, "/"); idx > 0 {
		switch name[:idx] {
		case "github.com":
			parts := strings.Split(name, "/")
			if parts < 3 {
				return ""
			}
			return "https://" + strings.Join(name[0:3], "/")
		case "k8s.io":
			repo := name[idx+1:]
			if i := strings.Index(repo, "/"); i > 0 {
				repo = repo[:i]
			}
			return "https://github.com/kubernetes/" + repo
		case "sigs.k8s.io":
			repo := name[idx+1:]
			if i := strings.Index(repo, "/"); i > 0 {
				repo = repo[:i]
			}
			return "https://github.com/kubernetes-sigs/" + repo
		case "gopkg.in":
			// gopkg.in/pkg.v3      → github.com/go-pkg/pkg (branch/tag v3, v3.N, or v3.N.M)
			// gopkg.in/user/pkg.v3 → github.com/user/pkg   (branch/tag v3, v3.N, or v3.N.M)
		case "golang.org":
		}
	}
	return ""
}

func parseVendorConfDependencies(r io.Reader) ([]dependency, error) {
	var deps []dependency
	re, err := regexp.Compile("[0-9a-f]{40}")
	if err != nil {
		return nil, err
	}

	s := bufio.NewScanner(r)
	for s.Scan() {
		ln := sanitizeLine(s.Text(), "#")
		if ln == "" {
			continue
		}
		parts := strings.Fields(ln)
		if len(parts) != 2 && len(parts) != 3 {
			return nil, fmt.Errorf("invalid config format: %s", ln)
		}

		var gitURL string
		if len(parts) == 3 {
			gitURL = parts[2]
		} else {
			gitURL = getGitURL(parts[0])
		}

		// trim the commit to 12 characters to match go mod length
		commitOrVersion := parts[1]
		var sha string
		if matched := re.Match([]byte(commitOrVersion)); matched {
			commitOrVersion = commitOrVersion[:12]
			sha = commitOrVersion
		}

		deps = append(deps, dependency{
			Name:   parts[0],
			Ref:    commitOrVersion,
			Sha:    sha,
			GitURL: gitURL,
		})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return deps, nil
}

func changelog(previous, commit string) ([]*change, error) {
	raw, err := getChangelog(previous, commit)
	if err != nil {
		return nil, err
	}
	return parseChangelog(raw)
}

func gitChangeDiff(previous, commit string) string {
	if previous != "" {
		return fmt.Sprintf("%s..%s", previous, commit)
	}
	return commit
}

func getChangelog(previous, commit string) ([]byte, error) {
	return git("log", "--oneline", "--topo-order", gitChangeDiff(previous, commit))
}

type changeProcessor interface {
	process(*change) error
}

func parseChangelog(changelog []byte) ([]*change, error) {
	var (
		changes []*change
		s       = bufio.NewScanner(bytes.NewReader(changelog))
	)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		changes = append(changes, &change{
			Commit:      fields[0],
			Description: strings.Join(fields[1:], " "),
		})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return changes, nil
}

func getSha(gitURL, rev string, cache Cache) (string, error) {
	key := fmt.Sprintf("git ls-remote %s %s %s^{}", gitURL, rev, rev)
	if b, ok := cache.Get(key); ok {
		logrus.WithField("cache", "hit").Debug(key)
		return string(b), nil
	}
	logrus.WithField("cache", "miss").Debug(key)

	b, err := git("ls-remote", gitURL, rev, rev+"^{}")
	if err != nil {
		logrus.WithError(err).WithField("key", key).Debug("not using sha")
		// Not found, don't use sha
		return "", nil
	}

	var (
		s        = bufio.NewScanner(bytes.NewReader(b))
		sha      string
		resolved bool
	)

	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.HasSuffix(fields[1], "^{}") {
			resolved = true
		} else if resolved {
			continue
		}
		sha = fields[0]
		if len(sha) > 12 {
			sha = sha[:12]
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	if sha == "" {
		return "", errors.New("revision not found")
	}

	cache.Put(key, []byte(sha))
	return sha, nil
}

func fileFromRev(rev, file string) (io.Reader, error) {
	p, err := git("show", fmt.Sprintf("%s:%s", rev, file))
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(p), nil
}

var gitConfigs = map[string]string{}

func git(args ...string) ([]byte, error) {
	var gitArgs []string
	for k, v := range gitConfigs {
		gitArgs = append(gitArgs, "-c", fmt.Sprintf("%s=%s", k, v))
	}
	gitArgs = append(gitArgs, args...)
	o, err := exec.Command("git", gitArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %s", err, o)
	}
	return o, nil
}

func renameDependencies(deps []dependency, renames map[string]projectRename) {
	if len(renames) == 0 {
		return
	}
	type dep struct {
		shortname string
		name      string
	}
	renameMap := map[string]dep{}
	for shortname, rename := range renames {
		renameMap[rename.Old] = dep{
			shortname: shortname,
			name:      rename.New,
		}
	}
	for i := range deps {
		if updated, ok := renameMap[deps[i].Name]; ok {
			logrus.Debugf("Renamed %s from %s to %s", updated.shortname, deps[i].Name, updated.name)
			deps[i].Name = updated.name
		}
	}
}

func getUpdatedDeps(previous, deps []dependency, ignored []string, cache Cache) ([]dependency, error) {
	var updated []dependency
	pm, cm := toDepMap(previous), toDepMap(deps)
	ignoreMap := map[string]struct{}{}
	for _, name := range ignored {
		ignoreMap[name] = struct{}{}
	}

	for name, c := range cm {
		if _, ok := ignoreMap[name]; ok {
			continue
		}
		d, ok := pm[name]
		if !ok {
			// it is a new dep and should be noted
			updated = append(updated, c)
			continue
		}
		// it exists, see if its updated
		if d.Ref != c.Ref {
			if d.Sha == "" {
				if d.GitURL == "" {
					gitURL, err := resolveGitURL(name, cache)
					if err != nil {
						return nil, errors.Wrapf(err, "git url for %q", name)
					}
					d.GitURL = gitURL
					if c.GitURL == "" {
						c.GitURL = d.GitURL
					}
				}
				sha, err := getSha(d.GitURL, d.Ref, cache)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to get sha for %q", name)
				}
				d.Sha = sha
			}
			if c.Sha == "" {
				if c.GitURL == "" {
					gitURL, err := resolveGitURL(name, cache)
					if err != nil {
						return nil, errors.Wrapf(err, "git url for %q", name)
					}
					c.GitURL = gitURL
				}
				sha, err := getSha(c.GitURL, c.Ref, cache)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to get sha for %q", name)
				}
				c.Sha = sha
			}

			if d.Sha != c.Sha {
				logrus.Debugf("Updated dependency: %q %s(%s) -> %s(%s)", d.Name, d.Ref, d.Sha, c.Ref, c.Sha)
				// set the previous commit
				c.Previous = d.Ref
				updated = append(updated, c)
			}
		}
	}
	return updated, nil
}

func toDepMap(deps []dependency) map[string]dependency {
	out := make(map[string]dependency)
	for _, d := range deps {
		out[d.Name] = d
	}
	return out
}

func addContributors(previous, commit string, contributors map[string]contributor) error {
	raw, err := git("log", `--format=%aE %aN`, gitChangeDiff(previous, commit))
	if err != nil {
		return err
	}
	s := bufio.NewScanner(bytes.NewReader(raw))
	for s.Scan() {
		p := strings.SplitN(s.Text(), " ", 2)
		if len(p) != 2 {
			return fmt.Errorf("unparsable git log output: %s", s.Text())
		}
		addContributor(contributors, p[1], p[0])
	}
	return s.Err()
}

func addContributor(contributors map[string]contributor, name, email string) {
	c, ok := contributors[email]
	if ok {
		c.Commits = c.Commits + 1
		if c.Name != name {
			var found bool
			for _, on := range c.OtherNames {
				if on == name {
					found = true
					break
				}
			}
			if !found {
				c.OtherNames = append(c.OtherNames, name)
			}
		}
	} else {
		c = contributor{
			Name:    name,
			Email:   email,
			Commits: 1,
		}
	}
	contributors[email] = c
}

func orderContributors(contributors map[string]contributor) []contributor {
	all := make([]contributor, 0, len(contributors))
	for _, c := range contributors {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Commits == all[j].Commits {
			return all[i].Name < all[j].Name
		}
		return all[i].Commits > all[j].Commits
	})

	nameEmail := map[string]string{}
	suggestions := []string{}
	for i := range all {
		logrus.Debugf("Contributor: %s <%s> with %d commits", all[i].Name, all[i].Email, all[i].Commits)
		for _, otherName := range all[i].OtherNames {
			suggestions = append(suggestions, fmt.Sprintf("\"%s <%s>\" also has name %q", all[i].Name, all[i].Email, otherName))
		}
		if email, ok := nameEmail[all[i].Name]; ok {
			suggestions = append(suggestions, fmt.Sprintf("\"%s <%s> <%s>\" has multiple emails", all[i].Name, email, all[i].Email))
		} else {
			nameEmail[all[i].Name] = all[i].Email
		}
	}
	for _, suggestion := range suggestions {
		logrus.Info("Mailmap suggestion: " + suggestion)
	}

	return all
}

// getTemplate will use a builtin template if the template is not specified on the cli
func getTemplate(context *cli.Context) (string, error) {
	path := context.String("template")
	f, err := os.Open(path)
	if err != nil {
		// if the template file does not exist and the path is for the default template then
		// return the compiled in template
		if os.IsNotExist(err) && path == defaultTemplateFile {
			return releaseNotes, nil
		}
		return "", err
	}
	defer f.Close()
	data, err := ioutil.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func resolveGitURL(name string, cache Cache) (string, error) {
	u := "https://" + name + "?go-get=1"
	if b, ok := cache.Get(u); ok {
		return string(b), nil
	}

	resp, err := http.Get(u)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", errors.Errorf("unexpected status code %d for %s", resp.StatusCode, u)
	}

	t := html.NewTokenizer(resp.Body)
	for {
		switch t.Next() {
		case html.ErrorToken:
			err := t.Err()
			if err == nil {
				err = errors.New("no go-import meta tag")
			}
			return "", err
		case html.StartTagToken, html.SelfClosingTagToken:
			var (
				tok           = t.Token()
				name, content string
			)
			for _, attr := range tok.Attr {
				if attr.Key == "name" {
					name = attr.Val
				} else if attr.Key == "content" {
					content = attr.Val
				}
			}
			if name == "go-import" {
				parts := strings.Fields(content)
				if len(parts) == 3 && parts[1] == "git" {
					resolved := parts[2]
					cache.Put(u, []byte(resolved))
					return resolved, nil
				}
			}
		}
	}
}
