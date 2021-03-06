package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/pkg/errors"
)

var (
	constraintArgs  rawConstraints
	enforceArgs     rawConstraints
	matchImagePaths bool
	tagPatternRaw   string
)

const (
	helpConstraint     = "perform semver update on given docker image to satisfy semver constraint (repeatable)"
	helpEnforce        = "override given docker image to enforce a semver constraint (repeatable)"
	hepMatchImagePaths = "allow path-based matching on docker images provided in -constraint and -enforce (e.g. 'foo/bar/star' matches 'foo/bar' and 'foo')"
	helpTagPattern     = "regex pattern for matching tags"
)

func main() {
	flag.Usage = func() {
		fmt.Printf(`update-docker-tags

Usage:
	update-docker-tags [options] < FILE | FOLDER >...

Options:
	-constraint          %s 
	-enforce             %s
	-match-image-paths   %s
	-tag-pattern         %s

Examples:

	Update all image tags in a directory:

	$ update-docker-tags dir/

	Update all image tags in the given files and folders, satisfying constraints:

	$ update-docker-tags -constraint=ubuntu=<18.04 -constraint=alpine=<3.10 deployment.yaml dir/ 

	Override all tags in the given files and folders to enforce a constraint:

	$ update-docker-tags -enforce=sourcegraph/frontend=~3.19 dir/

	Override all tags in Sourcegraph images (e.g. 'index.docker.io/sourcegraph/foo', 'index.docker.io/sourcegraph/bar'):

	$ update-docker-tags -match-image-paths -tag-pattern="[^index.docker.io](sourcegraph/.+):(.+)@(sha256:[[:alnum:]]+))" -enforce=sourcegraph=~3.19
`, helpConstraint, helpEnforce, hepMatchImagePaths, helpTagPattern)
		os.Exit(2)
	}
	flag.Var(&constraintArgs, "constraint", helpConstraint)
	flag.Var(&enforceArgs, "enforce", helpEnforce)
	flag.BoolVar(&matchImagePaths, "match-image-paths", false, hepMatchImagePaths)
	flag.StringVar(&tagPatternRaw, "tag-pattern", `(sourcegraph\/.+):(.+)@(sha256:[[:alnum:]]+)`, helpTagPattern)

	flag.Parse()

	parsedConstraints, err := constraintArgs.parse()
	if err != nil {
		log.Fatalf("failed to parse raw constraints, err: %s", err)
	}
	parsedEnforce, err := enforceArgs.parse()
	if err != nil {
		log.Fatalf("failed to parse raw enforce, err: %s", err)
	}

	paths := flag.Args()
	if len(paths) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	o := &options{
		constraints:     parsedConstraints,
		enforce:         parsedEnforce,
		filePaths:       paths,
		matchImagePaths: matchImagePaths,
	}

	for _, root := range o.filePaths {
		if err := updateDockerTags(o, root, regexp.MustCompile(tagPatternRaw)); err != nil {
			log.Fatalf("failed to update docker tags for root %q, err: %s", root, err)
		}
	}

}

// UpdateDockerTags updates the Docker tags for the entire file tree rooted at
// "root" using the provided constraints.
func updateDockerTags(o *options, root string, tagPattern *regexp.Regexp) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}

		if strings.HasPrefix(path, ".git") {
			// Highly doubt anyone would ever want us to traverse git directories.
			return nil
		}

		data, err := ioutil.ReadFile(path)
		if err != nil {
			return errors.Wrap(err, "when reading file contents")
		}

		printedPath := false

		// replaceErr is a workaround for replaceAllSubmatchFunc not propagating errors
		var replaceErr error
		data = replaceAllSubmatchFunc(tagPattern, data, func(groups [][]byte) [][]byte {

			repositoryName := string(groups[0])
			repository, err := newRepository(o, repositoryName)
			if err != nil {
				replaceErr = errors.Wrapf(err, "when initializing repository %q", repositoryName)
				return groups
			}

			originalTag := string(groups[1])
			var newTag string

			// if we are not enforcing a constraint, keep non-semver tags
			if isNonSemverTag(originalTag) && !repository.enforceConstraint {
				newTag = originalTag
			} else {
				latest, err := repository.findLatestSemverTag()
				if err != nil {
					replaceErr = errors.Wrapf(err, "when finding tag for '%s:%s'", repository.name, originalTag)
					return groups
				}

				newTag = latest
			}

			newDigest, err := repository.fetchImageDigest(newTag)
			if err != nil {
				replaceErr = errors.Wrapf(err, "when fetching image digest for '%s:%s'", repository.name, newTag)
				return groups
			}

			if !printedPath {
				printedPath = true
				fmt.Println(path)
			}

			fmt.Println("\t", repository.name, "\t\t", newTag)
			groups[1] = []byte(newTag)
			groups[2] = []byte(newDigest)

			return groups
		}, -1)

		if replaceErr != nil {
			return errors.Wrapf(replaceErr, "when replacing image tags in %q", path)
		}

		err = ioutil.WriteFile(path, data, info.Mode())
		return errors.Wrapf(err, "when writing file contents of %q", path)
	})
}

type repository struct {
	name              string
	constraint        *semver.Constraints
	enforceConstraint bool

	authToken string
}

func isNonSemverTag(tag string) bool {
	_, err := semver.NewVersion(tag)

	// Assume that "tag" isn't a semver one (like "latest")
	// if we're unable to parse it
	return err != nil
}

func (r *repository) findLatestSemverTag() (string, error) {
	var versions []*semver.Version
	tags, err := r.fetchAllTags()
	if err != nil {
		return "", errors.Wrap(err, "when fetching all tags")
	}

	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil {
			continue // ignore non-semver tags
		}

		if r.constraint != nil {
			if r.constraint.Check(v) {
				versions = append(versions, v)
			}
		} else {
			versions = append(versions, v)
		}
	}

	if len(versions) == 0 {
		if r.constraint != nil {
			return "", fmt.Errorf("no semver tags found for %q matching constraints %q", r.name, r.constraint.String())
		}
		return "", fmt.Errorf("no semver tags found for %q", r.name)
	}

	sort.Sort(sort.Reverse(semver.Collection(versions)))
	latestTag := versions[0].Original()
	return latestTag, nil
}

// Effectively the same as:
//
//  $ curl -s -D - -H "Authorization: Bearer $token" -H "Accept: application/vnd.docker.distribution.manifest.v2+json" https://index.docker.io/v2/sourcegraph/server/manifests/3.12.1 | grep Docker-Content-Digest
//
func (r *repository) fetchImageDigest(tag string) (string, error) {
	req, err := http.NewRequest("GET", "https://index.docker.io/v2/"+r.name+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.authToken))
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	return resp.Header.Get("Docker-Content-Digest"), nil
}

// Effectively the same as:
//
// 	$ export token=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:sourcegraph/server:pull" | jq -r .token)
//
func fetchAuthToken(repositoryName string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", repositoryName))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	result := struct {
		Token string
	}{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", err
	}
	return result.Token, nil
}

// Effectively the same as:
//
// 	$ curl -H "Authorization: Bearer $token" https://index.docker.io/v2/sourcegraph/server/tags/list
//
func (r *repository) fetchAllTags() ([]string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://index.docker.io/v2/%s/tags/list", r.name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.authToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	result := struct {
		Tags []string
	}{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return nil, err
	}
	return result.Tags, nil
}

// replaceAllSubmatchFunc is the missing regexp.ReplaceAllSubmatchFunc; to use it:
//
// 	pattern := regexp.MustCompile(...)
// 	data = replaceAllSubmatchFunc(pattern, data, func(groups [][]byte) [][]byte {
// 		// mutate groups here
// 		return groups
// 	})
//
// This snippet is MIT licensed. Please cite by leaving this comment in place. Find
// the latest version at:
//
//  https://gist.github.com/slimsag/14c66b88633bd52b7fa710349e4c6749
//
func replaceAllSubmatchFunc(re *regexp.Regexp, src []byte, repl func([][]byte) [][]byte, n int) []byte {
	var (
		result  = make([]byte, 0, len(src))
		matches = re.FindAllSubmatchIndex(src, n)
		last    = 0
	)
	for _, match := range matches {
		// Append bytes between our last match and this one (i.e. non-matched bytes).
		matchStart := match[0]
		matchEnd := match[1]
		result = append(result, src[last:matchStart]...)
		last = matchEnd

		// Determine the groups / submatch bytes and indices.
		groups := [][]byte{}
		groupIndices := [][2]int{}
		for i := 2; i < len(match); i += 2 {
			start := match[i]
			end := match[i+1]
			groups = append(groups, src[start:end])
			groupIndices = append(groupIndices, [2]int{start, end})
		}

		// Replace the groups as desired.
		groups = repl(groups)

		// Append match data.
		lastGroup := matchStart
		for i, newValue := range groups {
			// Append bytes between our last group match and this one (i.e. non-group-matched bytes)
			groupStart := groupIndices[i][0]
			groupEnd := groupIndices[i][1]
			result = append(result, src[lastGroup:groupStart]...)
			lastGroup = groupEnd

			// Append the new group value.
			result = append(result, newValue...)
		}
		result = append(result, src[lastGroup:matchEnd]...) // remaining
	}
	result = append(result, src[last:]...) // remaining
	return result
}

type rawConstraint struct {
	image      string
	constraint string
}

func (rc *rawConstraint) String() string {
	return fmt.Sprintf("%s=%s", rc.image, rc.constraint)
}

type rawConstraints []*rawConstraint

func (rc *rawConstraints) String() string {
	var elems []string
	for _, raw := range *rc {
		elems = append(elems, raw.String())
	}
	return strings.Join(elems, ", ")
}

func (rc *rawConstraints) Set(value string) error {
	splits := strings.Split(value, "=")
	if len(splits) != 2 {
		return fmt.Errorf("unable to split constraint %q", value)
	}

	image, constraint := splits[0], splits[1]
	*rc = append(*rc, &rawConstraint{
		image:      image,
		constraint: constraint,
	})
	return nil
}

func (rc *rawConstraints) parse() (map[string]*semver.Constraints, error) {
	out := map[string]*semver.Constraints{}
	for _, raw := range *rc {
		parsed, err := semver.NewConstraint(raw.constraint)
		if err != nil {
			return nil, fmt.Errorf("cannot parse constraint %q, err: %w", raw.constraint, err)
		}

		out[raw.image] = parsed
	}
	return out, nil
}

type options struct {
	constraints     map[string]*semver.Constraints
	enforce         map[string]*semver.Constraints
	filePaths       []string
	matchImagePaths bool
}

// newRepository generates a valid auth token for the given repository as well
// as any configured constraints/enforces for this repository.
//
// If matchImagePaths is enabled, we also provide constraints/enforces where
// the repositoryName has parents in its path that match configured constraints.
//
// For example, for repository `foo/bar/star` we also check if a constraint
// exists for `foo/bar` and `foo`
func newRepository(o *options, repositoryName string) (*repository, error) {
	token, err := fetchAuthToken(repositoryName)
	if err != nil {
		return nil, errors.Wrap(err, "when fetching auth token")
	}

	subpathIndex := len(repositoryName)
	for subpathIndex > 0 {
		match := repositoryName[:subpathIndex]
		if enforce, exists := o.enforce[match]; exists {
			return &repository{
				name:              repositoryName,
				constraint:        enforce,
				enforceConstraint: true,
				authToken:         token,
			}, nil
		}
		if constraint, exists := o.constraints[match]; exists {
			return &repository{
				name:       repositoryName,
				constraint: constraint,
				authToken:  token,
			}, nil
		}

		if o.matchImagePaths {
			subpathIndex = strings.LastIndex(match, "/")
		} else {
			break
		}
	}

	return &repository{
		name:       repositoryName,
		constraint: nil,
		authToken:  token,
	}, nil
}
