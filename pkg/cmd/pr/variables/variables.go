package variables

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Adds Pull Request environment variables to the .jx/variables.sh file
`)

	cmdExample = templates.Examples(`
		# add variables from the Pull Request and labels to the .jx/variables.sh file
		jx gitops pr variables

		# add variables from the Pull Request, labels and comments of the form '/jx-var FOO=bar' to the .jx/variables.sh file
		jx gitops pr variables --comments
	`)
)

// Options the options for the command
type Options struct {
	options.BaseOptions
	scmhelpers.PullRequestOptions

	UseComments      bool
	CommentPrefix    string
	EnvVarNamePrefix string
	File             string
	Result           *scm.PullRequest
}

// NewCmdPullRequestVariables creates a command object for the command
func NewCmdPullRequestVariables() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "variables",
		Short:   "Adds Pull Request environment variables to the .jx/variables.sh file",
		Long:    cmdLong,
		Aliases: []string{"var", "variable"},
		Example: cmdExample,
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	o.PullRequestOptions.AddFlags(cmd)
	cmd.Flags().StringVarP(&o.File, "file", "f", filepath.Join(".jx", "variables.sh"), "the default variables file to lazily create or enrich")
	cmd.Flags().StringVarP(&o.CommentPrefix, "comment-prefix", "", "/jx-var", "the comment prefix to specify environment variables")
	cmd.Flags().StringVarP(&o.EnvVarNamePrefix, "env-prefix", "", "PR_COMMENT_", "the prefix added to any variable name defined via a comment. e.g. a comment of '/jx-var CHEESE=edam' would generate 'export PR_COMMENT_CHEESE=edam'")
	cmd.Flags().BoolVarP(&o.UseComments, "comments", "", false, "if enabled query all the comments on the Pull Request and find any variables using special comments starting with the comment prefix")

	return cmd, o
}

// Run implements the command
func (o *Options) Run() error {
	err := o.PullRequestOptions.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to ")
	}
	pr, err := o.DiscoverPullRequest()
	if err != nil {
		return errors.Wrapf(err, "failed to discover the pull request")
	}
	if pr == nil {
		return errors.Errorf("no Pull Request could be found for %d in repository %s", o.Number, o.Repository)
	}
	return o.displayPullRequest(pr)
}

func (o *Options) displayPullRequest(pr *scm.PullRequest) error {
	o.Result = pr

	e := map[string]string{
		"PR_BASE_SHA": pr.Base.Sha,
		"PR_BASE_REF": pr.Base.Ref,
		"PR_HEAD_REF": pr.Head.Ref,
		"PR_HEAD_SHA": pr.Head.Sha,
	}

	for _, label := range pr.Labels {
		n := strings.ReplaceAll(label.Name, "/", "_")
		n = strings.ReplaceAll(n, ":", "_")
		n = strings.ReplaceAll(n, "-", "_")
		n = "PR_LABEL_" + strings.ToUpper(n)
		e[n] = "true"
	}

	if o.UseComments {
		err := o.loadPRComments(e, pr)
		if err != nil {
			return errors.Wrapf(err, "failed to load variables from PR comments")
		}
	}

	var lines []string
	for k, v := range e {
		lines = append(lines, fmt.Sprintf("export %s=\"%s\"", k, v))
	}
	sort.Strings(lines)
	return o.modifyVariables(strings.Join(lines, "\n"))

}

func (o *Options) modifyVariables(text string) error {
	err := o.BaseOptions.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate base options")
	}

	err = o.PullRequestOptions.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate PR options")
	}

	file := o.File
	if o.Dir != "" {
		file = filepath.Join(o.Dir, file)
	}
	exists, err := files.FileExists(file)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", file)
	}
	source := ""

	if exists {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			return errors.Wrapf(err, "failed to read file %s", file)
		}
		source = string(data)
	}

	buf := strings.Builder{}
	buf.WriteString("\n# generated by: jx gitops pr variables\n")

	buf.WriteString(text)
	buf.WriteString("\n")

	if source != "" {
		buf.WriteString("\n\n# content from git...\n")
		buf.WriteString(source)
	}

	source = buf.String()
	dir := filepath.Dir(file)
	err = os.MkdirAll(dir, files.DefaultDirWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to create dir %s", dir)
	}
	err = ioutil.WriteFile(file, []byte(source), files.DefaultFileWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to save %s", file)
	}
	log.Logger().Infof("added variables to file: %s", info(file))
	return nil
}

func (o *Options) loadPRComments(envVars map[string]string, pr *scm.PullRequest) error {
	ctx := o.GetContext()
	prNumber := pr.Number
	opts := scm.ListOptions{
		Sort: "asc",
	}
	comments, _, err := o.ScmClient.PullRequests.ListComments(ctx, o.FullRepositoryName, prNumber, opts)
	if err != nil {
		return errors.Wrapf(err, "failed to list comments of PR %d", prNumber)
	}
	for _, c := range comments {
		o.parseComments(envVars, c)
	}
	return nil
}

func (o *Options) parseComments(envVars map[string]string, c *scm.Comment) {
	lines := strings.Split(c.Body, "\n")
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if text == "" || !strings.HasPrefix(text, o.CommentPrefix) {
			continue
		}
		expression := strings.TrimSpace(strings.TrimPrefix(text, o.CommentPrefix))
		parts := strings.SplitN(expression, "=", 2)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		// trim quotes around the value
		value := strings.TrimSpace(parts[1])
		if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
			value = value[1 : len(value)-1]
		}
		envVars[o.EnvVarNamePrefix+name] = value
	}
}
