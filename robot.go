package main

import (
	"encoding/base64"
	"fmt"
	"k8s.io/apimachinery/pkg/util/sets"
	"regexp"
	"sigs.k8s.io/yaml"
	"strings"

	"github.com/opensourceways/community-robot-lib/config"
	framework "github.com/opensourceways/community-robot-lib/robot-gitee-framework"
	sdk "github.com/opensourceways/go-gitee/gitee"
	"github.com/sirupsen/logrus"
)

const botName = "welcome"

const (
	forIssueReply = `Hi ***%s***, 
if you want to get quick review about your issue, please contact the owner in first: @%s ,
and then any of the maintainers: @%s ,
and then any of the committers: @%s ,
if you have any question, please contact the SIG:%s.`

	forPRReply = `Hi ***%s***, 
if you want to get quick review about your pull request, please contact the owner in first: @%s ,
and then any of the maintainers: @%s ,
and then any of the committers: @%s ,
if you have any question, please contact the SIG:%s.`

	sigLink = `[%s](%s)`
)

var (
	sigLabelRegex = regexp.MustCompile(`(?m)^/sig\s*(.*?)\s*$`)
)

type iClient interface {
	CreatePRComment(owner, repo string, number int32, comment string) error
	CreateIssueComment(owner, repo string, number string, comment string) error
	GetBot() (sdk.User, error)
	GetIssueLabels(org, repo, number string) ([]sdk.Label, error)
	GetPullRequestChanges(org, repo string, number int32) ([]sdk.PullRequestFiles, error)
	AddMultiPRLabel(org, repo string, number int32, label []string) error
	GetPathContent(org, repo, path, ref string) (sdk.Content, error)
}

func newRobot(cli iClient) *robot {
	return &robot{cli: cli}
}

type robot struct {
	cli iClient
}

func (bot *robot) NewConfig() config.Config {
	return &configuration{}
}

func (bot *robot) getConfig(cfg config.Config, org, repo string) (*botConfig, error) {
	c, ok := cfg.(*configuration)
	if !ok {
		return nil, fmt.Errorf("can't convert to configuration")
	}
	if bc := c.configFor(org, repo); bc != nil {
		return bc, nil
	}

	return nil, fmt.Errorf("no config for this repo:%s/%s", org, repo)
}

func (bot *robot) RegisterEventHandler(p framework.HandlerRegitster) {
	p.RegisterPullRequestHandler(bot.handlePREvent)
	p.RegisterNoteEventHandler(bot.handleNoteEvent)
}

func (bot *robot) handlePREvent(e *sdk.PullRequestEvent, c config.Config, log *logrus.Entry) error {
	// when pr's label has been changed
	if sdk.GetPullRequestAction(e) != sdk.PRActionUpdatedLabel {
		return nil
	}

	org, repo := e.GetOrgRepo()
	author := e.GetPRAuthor()
	msgs := make([]string, 0)

	for l := range e.GetPRLabelSet() {
		if len(msgs) > 0 {
			break
		}
		if strings.HasPrefix(l, "sig/") {
			msg, err := bot.genSpecialWelcomeMessage(org, repo, author, l, e.GetPRNumber())
			if err != nil {
				return err
			}

			msgs = append(msgs, msg)
		}
	}

	comment := fmt.Sprintf("%s", msgs)

	return bot.cli.CreatePRComment(org, repo, e.GetPRNumber(), comment)
}

func (bot *robot) handleNoteEvent(e *sdk.NoteEvent, c config.Config, log *logrus.Entry) error {
	if !e.IsCreatingCommentEvent() {
		return nil
	}

	if e.IsPullRequest() {
		return nil
	}

	org, repo := e.GetOrgRepo()
	number := e.GetIssueNumber()
	author := e.GetIssueAuthor()

	comment := e.GetComment().GetBody()
	if !sigLabelRegex.MatchString(comment) {
		return nil
	}

	labels, err := bot.cli.GetIssueLabels(org, repo, number)
	if err != nil {
		return err
	}

	sigNames := make(map[string]string, 0)
	// var repositories []RepoMember
	repositories := make(map[string][]RepoMember, 0)
	for _, l := range labels {
		if len(sigNames) > 0 {
			break
		}

		if strings.HasPrefix(l.Name, "sig/") {
			sigs, err := bot.decodeSigsContent()
			if err != nil {
				return err
			}

			for _, sig := range sigs.Sigs {
				if l.Name == sig.SigLabel {
					sigNames[sig.Name] = sig.SigLink
					repositories[sig.Name] = sig.Repos
				}
			}
		}
	}

	// firstly @ who to resolve this problem
	owner := make([]string, 0)
	for _, r := range repositories {
		for _, rp := range r {
			for _, rps := range rp.Repo {
				if repo == rps {
					for _, o := range rp.Owner {
						owner = append(owner, o.GiteeID)
					}
				}
			}
		}
	}
	fmt.Println("owner is ", owner)

	// secondly @ persons to resolve
	//maintainers := make([]string, 0)
	//committers := make([]string, 0)
	//for k := range sigNames {
	//	os, cs, err := bot.decodeOWNERSContent(k)
	//	if err != nil {
	//		return err
	//	}
	//
	//	maintainers = append(maintainers, os...)
	//	committers = append(committers, cs...)
	//}

	// remove duplicate
	//for _, o := range owner {
	//	for i, j := range maintainers {
	//		if o == j {
	//			maintainers = append(maintainers[:i], maintainers[:i+1]...)
	//		}
	//	}
	//	for m, n := range committers {
	//		if o == n {
	//			committers = append(committers[:m], committers[:m+1]...)
	//		}
	//	}
	//}

	// gen hyper link in messages
	sigsLinks := make([]string, 0)
	for k, v := range sigNames {
		sigsLinks = append(sigsLinks, fmt.Sprintf(sigLink, k, v))
	}

	if len(owner) == 0 {
		owner = append(owner, []string{"xiangxinyong", "zhangxubo"}...)
	}

	//message := fmt.Sprintf(forIssueReply, author, strings.Join(owner, " , @"),
	//	strings.Join(maintainers, " , @"), strings.Join(committers, " , @"),
	//	strings.Join(sigsLinks, ""))

	message := fmt.Sprintf(forIssueReply, author, strings.Join(owner, " , @"), "", "", "")

	return bot.cli.CreateIssueComment(org, repo, number, message)
}

func (bot *robot) genSpecialWelcomeMessage(org, repo, author, label string, number int32) (string, error) {
	// get pr changed files
	changes, err := bot.cli.GetPullRequestChanges(org, repo, number)
	if err != nil {
		return "", err
	}

	owners := sets.NewString()
	sigName := make(map[string]string, 0)
	for _, c := range changes {
		fileOwner, sig, link, err := bot.getFileOwner(label, c.Filename)
		if err != nil {
			return "", err
		}

		owners.Insert(fileOwner.UnsortedList()...)
		sigName[sig] = link
	}

	//secondToConnect := sets.NewString()
	//for sn := range sigName {
	//	mrs, err := bot.decodeOWNERSContent(sn)
	//	if err != nil {
	//		return "", err
	//	}
	//
	//	secondToConnect.Insert(mrs...)
	//}

	// gen hyper link in messages
	sigsLinks := make([]string, 0)
	for k, v := range sigName {
		sigsLinks = append(sigsLinks, fmt.Sprintf(sigLink, k, v))
	}

	if len(owners) == 0 {
		owners.Insert([]string{"xiangxinyong", "zhangxubo"}...)
	}

	//return fmt.Sprintf(forPRReply, strings.Join(sigsLinks, ""), firstToConnect.UnsortedList(), secondToConnect.UnsortedList()), nil
	return fmt.Sprintf(forPRReply, author, strings.Join(sigsLinks, ""), strings.Join(firstToConnect.UnsortedList(), " ,@"), ""), nil
}

func (bot *robot) getFileOwner(label, fileName string) (sets.String, string, string, error) {
	sigs, err := bot.decodeSigsContent()
	if err != nil {
		return nil, "", "", err
	}

	sigName := ""
	link := ""

	var sig Sig
	for _, s := range sigs.Sigs {
		if label == s.SigLabel {
			sig = s
			sigName = s.Name
			link = s.SigLink
		}
	}

	first := sets.NewString()
	for _, s := range sig.Files {
		for _, ff := range s.File {
			if fileName == ff {
				for _, o := range s.Owner {
					first.Insert(o.GiteeID)
				}
			}
		}
	}

	return first, sigName, link, nil
}

func (bot *robot) decodeSigsContent() (*SigYaml, error) {
	fileContent, err := bot.cli.GetPathContent("new-op", "community", "test2.yaml", "master")
	if err != nil {
		return nil, err
	}

	c, err := base64.StdEncoding.DecodeString(fileContent.Content)
	if err != nil {
		return nil, err
	}

	var sigs SigYaml
	err = yaml.Unmarshal(c, &sigs)
	if err != nil {
		return nil, err
	}

	return &sigs, nil
}

func (bot *robot) decodeOWNERSContent(sigName string) ([]string, []string, error) {
	fileContent, err := bot.cli.GetPathContent("opengauss", "tc", fmt.Sprintf("sigs/%s/OWNERS", sigName), "master")
	if err != nil {
		return nil, nil, err
	}

	c, err := base64.StdEncoding.DecodeString(fileContent.Content)
	if err != nil {
		return nil, nil, err
	}

	type OWNERS struct {
		Maintainers []string `json:"maintainers,omitempty"`
		Committers  []string `json:"committers,omitempty"`
	}

	var o OWNERS
	err = yaml.Unmarshal(c, &o)
	if err != nil {
		return nil, nil, err
	}

	owner := o.Maintainers
	committer := o.Committers

	return owner, committer, nil
}
