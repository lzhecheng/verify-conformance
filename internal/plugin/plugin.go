/*
Copyright 2024 The Kubernetes Authors.

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

package plugin

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	githubql "github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"sigs.k8s.io/yaml"

	"sigs.k8s.io/verify-conformance/internal/common"
	"sigs.k8s.io/verify-conformance/internal/suite"
)

const (
	PluginName = "verify-conformance"
)

var (
	productYAMLRequiredFieldDateTypes = []ProductYAMLField{
		{Field: "website_url"},
		{Field: "repo_url"},
		{Field: "documentation_url"},
	}
	managedPRLabels = []string{
		"conformance-product-submission",
		"not-conformance-product-submission",
		"not-verifiable",
		"release-documents-checked",
		"required-tests-missing",
		"evidence-missing",
		"unable-to-process",
	}
	managedPRLabelTemplatesWithVersion = []string{
		"release-%v",
		"no-failed-tests-%v",
		"tests-verified-%v",
	}
	managedPRLabelTemplatesWithFileName = []string{"missing-file-%v"}
	// TODO swap out for ldflag to override variable if built with Ko
	godogPaths = []string{"./features/", "./kodata/features/", "/var/run/ko/features/", "../../kodata/features/"}
)

type ProductYAMLField struct {
	Field string
}

type githubClient interface {
	CreateStatus(string, string, string, github.Status) error
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	CreateComment(org, repo string, number int, comment string) error
	ListIssueCommentsWithContext(ctx context.Context, org, repo string, number int) ([]github.IssueComment, error)
	BotUserChecker() (func(candidate string) bool, error)
	AddLabel(org, repo string, number int, label string) error
	RemoveLabel(org, repo string, number int, label string) error
	DeleteStaleComments(org, repo string, number int, comments []github.IssueComment, isStale func(github.IssueComment) bool) error
	QueryWithGitHubAppsSupport(context.Context, interface{}, map[string]interface{}, string) error
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
}

type PullRequest struct {
	Number     githubql.Int
	HeadRefOID githubql.String
	Author     struct {
		Login githubql.String
	}
	Repository struct {
		Name  githubql.String
		Owner struct {
			Login githubql.String
		}
	}
	Labels struct {
		Nodes []struct {
			Name githubql.String
		}
	} `graphql:"labels(first:100)"`
	Files struct {
		Nodes []struct {
			Path githubql.String
		}
	} `graphql:"files(first:10)"`
	Title   githubql.String
	Commits struct {
		Nodes []struct {
			Commit struct {
				Oid    githubql.String
				Status struct {
					Contexts []struct {
						Context githubql.String
						State   githubql.String
					}
				}
			}
		}
	} `graphql:"commits(first:5)"`
}

type IssueComment struct {
	ID   githubql.Int
	Body githubql.String
	User struct {
		Login githubql.String
	}
	HTMLURL   githubql.String
	CreatedAt time.Time
	UpdatedAt time.Time
}

type SearchQuery struct {
	RateLimit struct {
		Cost      githubql.Int
		Remaining githubql.Int
	}
	Search struct {
		PageInfo struct {
			HasNextPage githubql.Boolean
			EndCursor   githubql.String
		}
		Nodes []struct {
			PullRequest suite.PullRequestQuery `graphql:"... on PullRequest"`
		}
	} `graphql:"search(type: ISSUE, first: 100, after: $searchCursor, query: $query)"`
}

// HelpProvider constructs the PluginHelp for this plugin that takes into account enabled repositories.
// HelpProvider defines the type for the function that constructs the PluginHelp for plugins.
func HelpProvider(_ []config.OrgRepo) (*pluginhelp.PluginHelp, error) {
	return &pluginhelp.PluginHelp{
			Description: `The Verify Conformance Request plugin checks the content of PRs that request Conformance Certification for Kubernetes to see if they are internally consistent. So, for example, if the title of the PR contains a reference to a Kubernetes version then this plugin checks to see that the Sonobouy e2e test logs refer to the same version.`,
		},
		nil
}

func fetchFileFromURI(uri string) (content string, resp *http.Response, err error) {
	resp, err = http.Get(uri)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	return string(body), resp, nil
}

// takes a patchUrl from a githubClient.PullRequestChange and transforms it
// to produce the url that delivers the raw file associated with the patch.
// Tested for small files.
func rawURLForBlobURL(patchUrl string) string {
	fileUrl := strings.Replace(patchUrl, "github.com", "raw.githubusercontent.com", 1)
	fileUrl = strings.Replace(fileUrl, "/blob", "", 1)
	return fileUrl
}

// Executes the search query contained in q using the GitHub client ghc
func search(ctx context.Context, log *logrus.Entry, ghc githubClient, q string, org string) ([]suite.PullRequestQuery, error) {
	var ret []suite.PullRequestQuery
	vars := map[string]interface{}{
		"query":        githubql.String(q),
		"searchCursor": (*githubql.String)(nil),
	}
	var totalCost int
	var remaining int
	for {
		sq := SearchQuery{}
		log.Infof("query \"%s\" ", q)
		if err := ghc.QueryWithGitHubAppsSupport(ctx, &sq, vars, org); err != nil {
			return nil, err
		}
		fmt.Printf("%+v\n", sq)
		totalCost += int(sq.RateLimit.Cost)
		remaining = int(sq.RateLimit.Remaining)
		for _, n := range sq.Search.Nodes {
			ret = append(ret, n.PullRequest)
		}
		if !sq.Search.PageInfo.HasNextPage {
			break
		}
		vars["searchCursor"] = githubql.NewString(sq.Search.PageInfo.EndCursor)
	}
	log.Infof("Search for query \"%s\" cost %d point(s). %d remaining.", q, totalCost, remaining)
	return ret, nil
}

func NewPRSuiteForPR(log *logrus.Entry, ghc githubClient, pr *suite.PullRequestQuery) (prSuite *suite.PRSuite, err error) {
	prSuite = suite.NewPRSuite(&suite.PullRequest{PullRequestQuery: *pr})
	issueLabels, err := ghc.GetIssueLabels(string(pr.Repository.Owner.Login), string(pr.Repository.Name), int(pr.Number))
	if err != nil {
		return &suite.PRSuite{}, fmt.Errorf("error fetching PR issue labels for issue (%v), %v ", pr.Number, err)
	}
	for _, l := range issueLabels {
		prSuite.PR.Labels = append(prSuite.PR.Labels, l.Name)
	}

	stableTxt, err := common.GetStableTxt()
	if err != nil {
		return &suite.PRSuite{}, fmt.Errorf("unable to read latest version info")
	}
	prSuite.KubernetesReleaseVersionLatest = stableTxt

	var productYAMLContent string
	changes, err := ghc.GetPullRequestChanges(string(pr.Repository.Owner.Login), string(pr.Repository.Name), int(pr.Number))
	if err != nil {
		return &suite.PRSuite{}, fmt.Errorf("error fetching PR (%v) changes, %v", pr.Number, err)
	}
	for _, c := range changes {
		content, _, err := fetchFileFromURI(rawURLForBlobURL(c.BlobURL))
		if err != nil {
			return &suite.PRSuite{}, fmt.Errorf("error fetching content of '%v' in PR (%v) via '%v', %v", c.Filename, pr.Number, c.BlobURL, err)
		}

		baseName := path.Base(c.Filename)
		prFile := &suite.PullRequestFile{
			Name:     c.Filename,
			BaseName: baseName,
			BlobURL:  c.BlobURL,
			Contents: content,
		}
		prSuite.PR.SupportingFiles = append(prSuite.PR.SupportingFiles, prFile)

		if baseName == "PRODUCT.yaml" {
			productYAMLContent = content
		}
	}
	if productYAMLContent == "" {
		log.Printf("failed to find PRODUCT.yaml from the list of files in the PR (%v)", pr.Number)
		return prSuite, nil
	}

	productYAML := map[string]string{}
	err = yaml.Unmarshal([]byte(productYAMLContent), &productYAML)
	if err != nil {
		log.Printf("failed to parse content of PRODUCT.yaml in PR (%v), %v", pr.Number, err)
		return prSuite, nil
	}

	for _, f := range productYAMLRequiredFieldDateTypes {
		uri := productYAML[f.Field]
		if uri == "" {
			log.Printf("field '%v' is empty in PRODUCT.yaml, not resolving URL\n", f.Field)
			continue
		}
		if prSuite.PR.ProductYAMLURLDataTypes == nil {
			prSuite.PR.ProductYAMLURLDataTypes = map[string]string{}
		}
		prSuite.PR.ProductYAMLURLDataTypes[f.Field] = ""
		u, err := url.ParseRequestURI(uri)
		if err != nil {
			log.Printf("failed to parse url '%v' of the field '%v' in PRODUCT.yaml in PR (%v) as it is not a valid URL, %v", uri, f.Field, pr.Number, err)
			continue
		}
		if u.Scheme == "" {
			u.Scheme = "https"
		}
		req, err := http.NewRequest(http.MethodHead, u.String(), nil)
		if err != nil {
			log.Printf("failed to prepare new request for URL (%v) for PR (%v), %v", u, pr.Number, err)
			continue
		}
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := &http.Client{Transport: tr}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("failed to make a HEAD request to url '%v' from the field '%v' in PRODUCT.yaml in PR (%v), %v", u, f.Field, pr.Number, err)
			continue
		}
		contentType := resp.Header.Get("Content-Type")
		log.Printf("%v: '%v' -> %v = '%v'\n", pr.Number, f.Field, u.String(), contentType)
		prSuite.PR.ProductYAMLURLDataTypes[f.Field] = contentType
	}

	return prSuite, nil
}

func GetGodogPaths() (paths []string) {
	for _, p := range godogPaths {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		paths = append(paths, p)
	}
	return paths
}

func labelIsManaged(input string) bool {
	for _, l := range managedPRLabels {
		if l == input {
			return true
		}
	}
	return false
}

func labelIsVersionLabel(label, version string) bool {
	for _, ml := range managedPRLabelTemplatesWithVersion {
		if strings.Contains(label, strings.ReplaceAll(ml, "%v", "")) {
			return true
		}
		if fmt.Sprintf(ml, version) == label {
			return true
		}
	}
	return false
}

func labelIsFileLabel(label string, missingFiles []string) bool {
	for _, ml := range managedPRLabelTemplatesWithFileName {
		if strings.Contains(label, strings.ReplaceAll(ml, "%v", "")) {
			return true
		}
		for _, f := range missingFiles {
			if fmt.Sprintf(ml, f) == label {
				return true
			}
		}
	}
	return false
}

func updateLabels(log *logrus.Entry, ghc githubClient, pr *suite.PullRequestQuery, prSuite *suite.PRSuite, labels []string) (newLabels, removedLabels []string, err error) {
labels:
	for _, l := range labels {
		isManagedLabel := labelIsManaged(l)
		isInVersionLabel := labelIsVersionLabel(l, prSuite.KubernetesReleaseVersion)
		isInMissingFileLabel := labelIsFileLabel(l, prSuite.MissingFiles)
		log.Printf("label '%v', isManagedLabel %v, isInVersionLabel %v, isInMissingFileLabel %v\n", l, isManagedLabel, isInVersionLabel, isInMissingFileLabel)
		if !isInVersionLabel && !isInMissingFileLabel && !isManagedLabel {
			continue labels
		}
		foundInLabels := false
		for _, prl := range prSuite.PR.Labels {
			if prl == l {
				foundInLabels = true
			}
		}
		if foundInLabels {
			continue labels
		}
		if err := githubClient.AddLabel(ghc, string(pr.Repository.Owner.Login), string(pr.Repository.Name), int(pr.Number), l); err != nil {
			return []string{}, []string{}, fmt.Errorf("failed to add label '%v' to %v/%v!%v", l, pr.Repository.Owner.Login, pr.Repository.Name, pr.Number)
		}
		newLabels = append(newLabels, l)
	}
	prSuite.PR.Labels = append(prSuite.PR.Labels, newLabels...)

prLabels:
	for _, prl := range prSuite.PR.Labels {
		isManagedLabel := labelIsManaged(prl)
		isInVersionLabel := labelIsVersionLabel(prl, prSuite.KubernetesReleaseVersion)
		isInMissingFileLabel := labelIsFileLabel(prl, prSuite.MissingFiles)
		log.Printf("label '%v', isManagedLabel %v, isInVersionLabel %v, isInMissingFileLabel %v\n", prl, isManagedLabel, isInVersionLabel, isInMissingFileLabel)
		if !isInVersionLabel && !isInMissingFileLabel && !isManagedLabel {
			continue prLabels
		}

		foundInLabels := false
		for _, l := range labels {
			if prl == l {
				foundInLabels = true
			}
		}
		if foundInLabels {
			continue prLabels
		}
		// log.Printf("Will remove label '%v'", prl)
		if err := githubClient.RemoveLabel(ghc, string(pr.Repository.Owner.Login), string(pr.Repository.Name), int(pr.Number), prl); err != nil {
			return []string{}, []string{}, fmt.Errorf("failed to add remove '%v' to %v/%v!%v", prl, pr.Repository.Owner.Login, pr.Repository.Name, pr.Number)
		}
		removedLabels = append(prSuite.PR.Labels, prl)
	}
	prSuite.PR.Labels = removeSliceOfStringsFromStringSlice(prSuite.PR.Labels, removedLabels)

	return newLabels, removedLabels, nil
}

func updateComments(log *logrus.Entry, ghc githubClient, pr *suite.PullRequestQuery, prSuite *suite.PRSuite, comment string) error {
	comments, err := githubClient.ListIssueCommentsWithContext(ghc, context.TODO(), string(pr.Repository.Owner.Login), string(pr.Repository.Name), int(pr.Number))
	if err != nil {
		return fmt.Errorf("unable to list comments, %v", err)
	}
	botUserChecker, err := githubClient.BotUserChecker(ghc)
	if err != nil {
		return fmt.Errorf("unable to get bot name, %v", err)
	}
	botComments := []github.IssueComment{}
	for _, c := range comments {
		if !botUserChecker(c.User.Login) {
			continue
		}
		if c.Body == "" {
			continue
		}
		botComments = append(botComments, c)
	}
	if len(botComments) > 0 && botComments[len(botComments)-1].Body == comment {
		log.Printf("warning: nothing new to add in PR (%v)\n", int(pr.Number))
		return nil
	}
	botCommentsToPrune := botComments
	if len(botComments) > 0 {
		botCommentsToPrune = botComments[:len(botComments)-1]
	}

	err = githubClient.DeleteStaleComments(
		ghc,
		string(pr.Repository.Owner.Login),
		string(pr.Repository.Name),
		int(pr.Number),
		botCommentsToPrune,
		func(ic github.IssueComment) bool {
			return botUserChecker(ic.User.Login)
		},
	)
	if err != nil {
		return fmt.Errorf("unable to prune stale comments comments on PR (%v), %v", int(pr.Number), err)
	}

	err = githubClient.CreateComment(ghc, string(pr.Repository.Owner.Login), string(pr.Repository.Name), int(pr.Number), comment)
	if err != nil {
		return err
	}
	return nil
}

func removeSliceOfStringsFromStringSlice(originalSlice []string, removeSlice []string) (output []string) {
o:
	for _, oItem := range originalSlice {
		for _, delString := range removeSlice {
			if oItem == delString {
				continue o
			}
		}
		output = append(output, oItem)
	}
	return output
}

func isConformancePR(pr *suite.PullRequestQuery) bool {
	return strings.Contains(strings.ToLower(string(pr.Title)), "conformance results for")
}

func updateStatus(log *logrus.Entry, ghc githubClient, pr *suite.PullRequestQuery, prSuite *suite.PRSuite, state string) error {
	var description string
	currentLatestHasCurrentStatus := false
commitLoop:
	for _, commit := range pr.Commits.Nodes {
		if string(commit.Commit.Oid) != string(pr.HeadRefOID) {
			continue
		}
		for _, context := range commit.Commit.Status.Contexts {
			if strings.EqualFold(string(context.Context), "verify-conformance") {
				currentLatestHasCurrentStatus = strings.EqualFold(string(context.State), string(githubql.StatusStateSuccess))
				break commitLoop
			}
		}
	}
	if currentLatestHasCurrentStatus {
		log.Infof("PR %v has status up to date", pr.Number)
		return nil
	}
	switch state {
	case "success":
		description = "All checks are passing"
	case "failure":
		description = "Please check failing requirements and update accordingly"
	default:
		description = "Internal error"
		log.Infof("PR %v has invalid state", pr.Number)
	}
	log.Infof("PR %v setting state of '%v' with description '%v'", pr.Number, state, description)
	cs, err := ghc.GetCombinedStatus(string(pr.Repository.Owner.Login), string(pr.Repository.Name), string(pr.HeadRefOID))
	if err != nil {
		log.Infof("PR %v failed to get combined status: %v", pr.Number, err)
		return err
	}
	if cs.SHA == string(pr.HeadRefOID) && cs.State == state {
		log.Infof("PR %v state unchanged", pr.Number)
		return nil
	}
	if err := ghc.CreateStatus(
		string(pr.Repository.Owner.Login), string(pr.Repository.Name), string(pr.HeadRefOID),
		github.Status{
			Context:     "verify-conformance",
			State:       state,
			Description: description,
		}); err != nil {
		log.Infof("PR %v failed to create status: %v", pr.Number, err)
		return err
	}
	return nil
}

// handle checks a Conformance Certification PR to determine if the contents of the PR pass sanity checks.
// Adds a comment to indicate whether or not the version in the PR title occurs in the supplied logs.
func handle(log *logrus.Entry, ghc githubClient, pr *suite.PullRequestQuery) error {
	godogFeaturePaths := GetGodogPaths()
	prSuite, err := NewPRSuiteForPR(log, ghc, pr)
	if err != nil {
		return err
	}
	prSuite.MetadataFolder = path.Join(common.GetDataPath(), "conformance-testdata")
	prSuite.SetSubmissionMetadatafromFolderStructure()
	if !isConformancePR(pr) {
		log.Printf("This PR (%v) is not a conformance PR\n", int(pr.Number))
		finalComment := strings.Join(
			[]string{
				"This pull request appears to not be a conformance results submission; Checks will not run.",
				"",
				"If this change is intended to be verified as a conformance results submission see: " +
					"[_content of the PR_](https://github.com/cncf/k8s-conformance/blob/master/instructions.md#contents-of-the-pr), " +
					"and [_requirements_](https://github.com/cncf/k8s-conformance/blob/master/instructions.md#requirements)",
			},
			"\n")
		labels := []string{"not-conformance-product-submission", "unable-to-process"}
		state := "pending"
		if _, _, err := updateLabels(log, ghc, pr, prSuite, labels); err != nil {
			return err
		}
		if err := updateComments(log, ghc, pr, prSuite, finalComment); err != nil {
			return err
		}
		if err := updateStatus(log, ghc, pr, prSuite, state); err != nil {
			return err
		}
		return nil
	}

	if err := prSuite.ItIsAValidAndSupportedRelease(); err != nil {
		finalComment := err.Error()
		finalComment = fmt.Sprintf("%v.", strings.ToUpper(finalComment[:1])+finalComment[1:])
		labels := []string{"conformance-product-submission", "unable-to-process"}
		state := "pending"
		if _, _, err := updateLabels(log, ghc, pr, prSuite, labels); err != nil {
			return err
		}
		if err := updateComments(log, ghc, pr, prSuite, finalComment); err != nil {
			return err
		}
		if err := updateStatus(log, ghc, pr, prSuite, state); err != nil {
			return err
		}
		return fmt.Errorf("unable to process release file as it is missing for release %v", prSuite.KubernetesReleaseVersion)
	}
	conformanceYAMLFilePath := path.Join(prSuite.MetadataFolder, prSuite.KubernetesReleaseVersion, "conformance.yaml")
	if _, err := common.ReadFile(conformanceYAMLFilePath); err != nil && os.IsNotExist(err) {
		finalComment := fmt.Sprintf("The release version %v is unable to be processed at this time; Please wait as this version may become available soon.", prSuite.KubernetesReleaseVersion)
		labels := []string{"conformance-product-submission", "unable-to-process"}
		state := "pending"
		if _, _, err := updateLabels(log, ghc, pr, prSuite, labels); err != nil {
			return err
		}
		if err := updateComments(log, ghc, pr, prSuite, finalComment); err != nil {
			return err
		}
		if err := updateStatus(log, ghc, pr, prSuite, state); err != nil {
			return err
		}
		return fmt.Errorf("unable to process release file as it is missing for release %v", prSuite.KubernetesReleaseVersion)
	}
	prSuite.NewTestSuite(suite.PRSuiteOptions{Paths: godogFeaturePaths}).Run()

	finalComment, labels, state, err := prSuite.GetLabelsAndCommentsFromSuiteResultsBuffer()
	if err != nil {
		return err
	}
	if finalComment == "" && len(labels) == 0 {
		log.Printf("There is nothing new to comment on PR (%v)\n", int(prSuite.PR.Number))
		return nil
	}

	fmt.Printf("PR url: https://github.com/%v/%v/pull/%v \n", prSuite.PR.PullRequestQuery.Repository.Owner.Login, prSuite.PR.PullRequestQuery.Repository.Name, prSuite.PR.PullRequestQuery.Number)
	fmt.Println("PR title:", prSuite.PR.Title)
	fmt.Println("Release Version:", prSuite.KubernetesReleaseVersion)
	fmt.Println("Labels:", strings.Join(labels, ", "))
	fmt.Println(finalComment)

	newLabels, removedLabels, err := updateLabels(log, ghc, pr, prSuite, labels)
	if err != nil {
		return err
	}
	fmt.Println("NewLabels: ", newLabels)
	fmt.Println("RemovedLabels: ", removedLabels)

	if err := updateComments(log, ghc, pr, prSuite, finalComment); err != nil {
		return err
	}
	if err := updateStatus(log, ghc, pr, prSuite, state); err != nil {
		return err
	}
	return nil
}

func NewPullRequestQueryForGithubPullRequest(orgName string, repoName string, number int, pr *github.PullRequest) *suite.PullRequestQuery {
	return &suite.PullRequestQuery{
		Title:  githubql.String(pr.Title),
		Number: githubql.Int(number),
		Author: struct {
			Login githubql.String
		}{
			Login: githubql.String(pr.User.Login),
		},
		Repository: struct {
			Name  githubql.String
			Owner struct {
				Login githubql.String
			}
		}{
			Name: githubql.String(repoName),
			Owner: struct {
				Login githubql.String
			}{
				Login: githubql.String(pr.User.Login),
			},
		},
	}
}

func NewGitHubPullRequestForPullRequestQuery(orgName string, repoName string, number int, pr *suite.PullRequestQuery) *github.PullRequest {
	return &github.PullRequest{
		Title:  string(pr.Title),
		Number: number,
		User: github.User{
			Login: string(pr.Author.Login),
		},
	}
}

// HandlePullRequestEvent handles a GitHub pull request event
func HandlePullRequestEvent(log *logrus.Entry, ghc githubClient, pre *github.PullRequestEvent) error {
	log.Infof("HandlePullRequestEvent")
	switch pre.Action {
	case github.PullRequestActionOpened, github.PullRequestActionReopened:
	default:
		return nil
	}

	return handle(log, ghc, NewPullRequestQueryForGithubPullRequest(pre.Repo.Owner.Login, pre.Repo.Name, pre.Number, &pre.PullRequest))
}

// HandleIssueCommentEvent handles a GitHub issue comment event and adds or removes a
// message indicating that there are inconsitencies in the version of Kubernetes
// referenced in the title of the PR versus the log file evidence supplied in the associated commit.
func HandleIssueCommentEvent(log *logrus.Entry, ghc githubClient, ice *github.IssueCommentEvent) error {
	log.Infof("HandleIssueCommentEvent")
	if !ice.Issue.IsPullRequest() {
		return nil
	}
	pr, err := ghc.GetPullRequest(ice.Repo.Owner.Login, ice.Repo.Name, ice.Issue.Number)
	if err != nil {
		return err
	}

	return handle(log, ghc, NewPullRequestQueryForGithubPullRequest(ice.Repo.Owner.Login, ice.Repo.Name, ice.Issue.Number, pr))
}

// HandleAll is called periodically and the period is setup in main.go
// It runs a Github Query to get all open PRs for this repo which contains k8s conformance requests
//
// Each PR is checked in turn, we check
//   - for the presence of a Release Version in the PR title
//
// - then we take that version and verify that the e2e test logs refer to that same release version.
//
// if all is in order then we add the verifiable label and a release-Vx.y label
// if there is an inconsistency we add a comment that explains the problem
// and tells the PR submitter to review the documentation
func HandleAll(log *logrus.Entry, ghc githubClient, config *plugins.Configuration) error {
	log.Infof("%v : HandleAll : Checking all PRs for handling", PluginName)

	orgs, repos := config.EnabledReposForExternalPlugin(PluginName) // TODO : Overkill see below
	log.Infof("orgs: %#v, repos: %#v", orgs, repos)

	if len(orgs) == 0 && len(repos) == 0 {
		log.Warnf("HandleAll : No repos have been configured for the %s plugin", PluginName)
		return nil
	}

	var queryOpenPRs bytes.Buffer
	//	fmt.Fprint(&queryOpenPRs, "archived:false is:pr is:open -label:verifiable")
	fmt.Fprint(&queryOpenPRs, "archived:false is:pr is:open ")
	for _, repo := range repos {
		slashSplit := strings.Split(repo, "/")
		if n := len(slashSplit); n != 2 {
			log.WithField("repo", repo).Warn("Found repo that was not in org/repo format, ignoring...")
			continue
		}
		org := slashSplit[0]
		orgs = append(orgs, org)
		fmt.Fprintf(&queryOpenPRs, " repo:\"%s\"", repo)
	}

	prs := []suite.PullRequestQuery{}
	for _, org := range orgs {
		prSearch, err := search(context.Background(), log, ghc, queryOpenPRs.String(), org)
		if err != nil {
			return err
		}
		prs = append(prs, prSearch...)
	}
	log.Infof("Considering %d PRs.", len(prs))

	for _, pr := range prs {
		err := handle(log, ghc, &pr)
		if err != nil {
			log.Infof("error running checks on PR: %v", err)
		}
	}
	return nil
}
