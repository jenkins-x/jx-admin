package create

import (
	"fmt"
	"io/ioutil"
	"os"

	"github.com/jenkins-x/jx-remote/pkg/common"
	"github.com/jenkins-x/jx-remote/pkg/envfactory"
	"github.com/jenkins-x/jx-remote/pkg/githelpers"
	"github.com/jenkins-x/jx-remote/pkg/reqhelpers"
	"github.com/jenkins-x/jx-remote/pkg/rootcmd"
	"github.com/jenkins-x/jx/pkg/cmd/helper"
	"github.com/jenkins-x/jx/pkg/config"
	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/pkg/errors"

	"github.com/jenkins-x/jx/pkg/cmd/templates"
	"github.com/spf13/cobra"
)

var (
	createLong = templates.LongDesc(`
		Creates a new git repository for a new Jenkins X installation
`)

	createExample = templates.Examples(`
		# create a new git repository which we can then boot up
		%s create
	`)
)

// SupportHelm3ForDev for now lets disable using helm3 in development
const SupportHelm3ForDev = false

// CreateOptions the options for creating a repository
type CreateOptions struct {
	envfactory.EnvFactory
	DisableVerifyPackages bool
	Requirements          config.RequirementsConfig
	Flags                 reqhelpers.RequirementFlags
	Environment           string
	InitialGitURL         string
	Dir                   string
	RequirementsFile      string
	DevGitKind            string
	DevGitURL             string
	Cmd                   *cobra.Command
	Args                  []string
	AddApps               []string
	RemoveApps            []string
}

// NewCmdCreate creates a command object for the command
func NewCmdCreate() (*cobra.Command, *CreateOptions) {
	o := &CreateOptions{}

	cmd := &cobra.Command{
		Use:     "create",
		Short:   "Creates a new git repository for a new Jenkins X installation",
		Long:    createLong,
		Example: fmt.Sprintf(createExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			o.Cmd = cmd
			o.Args = args
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	o.Cmd = cmd

	cmd.Flags().StringVarP(&o.Environment, "env", "e", "", "The name of the remote environment to create")
	cmd.Flags().StringVarP(&o.InitialGitURL, "initial-git-url", "", "", "The git URL to clone to fetch the initial set of files for a helm 3 / helmfile based git configuration if this command is not run inside a git clone or against a GitOps based cluster")
	cmd.Flags().StringVarP(&o.DevGitKind, "dev-git-kind", "", "", "The kind of git server for the development environment")
	cmd.Flags().StringVarP(&o.DevGitURL, "dev-git-url", "", "", "The git URL of the development environment if you are creating a remote staging/production cluster. If specified this will create a Pull Request on the development cluster")
	cmd.Flags().StringVarP(&o.Dir, "dir", "", "", "The directory used to create the development environment git repository inside. If not specified a temporary directory will be used")
	cmd.Flags().StringVarP(&o.RequirementsFile, "requirements", "r", "", "The 'jx-requirements.yml' file to use in the created development git repository. This file may be created via terraform")
	cmd.Flags().StringArrayVarP(&o.AddApps, "add", "", nil, "The apps/charts to add to the `jx-apps.yml` file to add the apps")
	cmd.Flags().StringArrayVarP(&o.RemoveApps, "remove", "", nil, "The apps/charts to remove from the `jx-apps.yml` file to remove the apps")

	reqhelpers.AddRequirementsFlagsOptions(cmd, &o.Flags)
	reqhelpers.AddRequirementsOptions(cmd, &o.Requirements)

	o.EnvFactory.AddFlags(cmd)
	return cmd, o
}

// Run implements the command
func (o *CreateOptions) Run() error {
	if o.Gitter == nil {
		o.Gitter = gits.NewGitCLI()
	}

	if o.DevGitURL != "" {
		if o.Environment == "dev" {
			log.Logger().Warnf("you are creating a %s environment but are also trying to create a Pull Request on a development environment git repository %s - did you mean to do that?", util.ColorInfo(o.Environment), util.ColorInfo(o.DevGitURL))
		}
		if o.DevGitKind == "" {
			o.DevGitKind = gits.SaasGitKind(o.DevGitURL)
			if o.DevGitKind == "" {
				return errors.Errorf("missing git kind option: --dev-git-kind")
			}
		}
	}

	dir, err := o.gitCloneIfRequired(o.Gitter)
	if err != nil {
		return err
	}

	err = reqhelpers.OverrideRequirements(o.Cmd, o.Args, dir, o.RequirementsFile, &o.Requirements, &o.Flags, o.Environment)
	if err != nil {
		return errors.Wrapf(err, "failed to override requirements in dir %s", dir)
	}

	_, _, err = reqhelpers.ValidateApps(dir, o.AddApps, o.RemoveApps)
	if err != nil {
		return errors.Wrapf(err, "failed to validate the apps based on requirements in dir %s", dir)
	}

	err = o.EnvFactory.VerifyPreInstall(o.DisableVerifyPackages, dir)
	if err != nil {
		return errors.Wrapf(err, "failed to verify requirements in dir %s", dir)
	}

	log.Logger().Infof("created git source at %s", util.ColorInfo(dir))

	_, err = githelpers.AddAndCommitFiles(o.Gitter, dir, "fix: initial code")
	if err != nil {
		return errors.Wrap(err, "failed to add files to git")
	}
	err = o.EnvFactory.CreateDevEnvGitRepository(dir, o.Flags.EnvironmentGitPublic)
	if err != nil {
		return errors.Wrap(err, "failed to createt the environment git repository")
	}
	if o.DevGitURL != "" {
		return o.createPullRequestOnDevRepository(o.DevGitURL, o.DevGitKind)
	}
	return nil
}

// gitCloneIfRequired if the specified directory is already a git clone then lets just use it
// otherwise lets make a temporary directory and clone the git repository specified
// or if there is none make a new one
func (o *CreateOptions) gitCloneIfRequired(gitter gits.Gitter) (string, error) {
	gitURL := o.InitialGitURL
	if o.Environment == "" {
		if SupportHelm3ForDev {
			o.Environment = "dev"
		} else {
			return "", util.MissingOption("env")
		}
	}
	if gitURL == "" {
		if o.Environment == "dev" {
			gitURL = common.DefaultBootHelmfileRepository
		} else {
			gitURL = common.DefaultEnvironmentHelmfileGitRepoURL
		}
	}
	var err error
	dir := o.Dir
	if dir != "" {
		err = os.MkdirAll(dir, util.DefaultWritePermissions)
		if err != nil {
			return "", errors.Wrapf(err, "failed to create directory %s", dir)
		}
	} else {
		dir, err = ioutil.TempDir("", "helmboot-")
		if err != nil {
			return "", errors.Wrap(err, "failed to create temporary directory")
		}
	}

	log.Logger().Debugf("cloning %s to directory %s", util.ColorInfo(gitURL), util.ColorInfo(dir))

	err = gitter.Clone(gitURL, dir)
	if err != nil {
		return "", errors.Wrapf(err, "failed to clone repository %s to directory: %s", gitURL, dir)
	}
	return dir, nil
}

func (o *CreateOptions) createPullRequestOnDevRepository(gitURL string, kind string) error {
	cr := o.CreatedRepository
	if cr == nil {
		return errors.Errorf("no CreatedRepository available")
	}
	dir, err := ioutil.TempDir("", "helmboot-")
	if err != nil {
		return errors.Wrap(err, "failed to create temporary directory")
	}
	err = o.Gitter.Clone(gitURL, dir)
	if err != nil {
		return errors.Wrapf(err, "failed to clone repository %s to directory: %s", gitURL, dir)
	}
	requirements, fileName, err := config.LoadRequirementsConfig(dir)
	if err != nil {
		return errors.Wrapf(err, "failed to load requirements file in git clone of %s in  directory: %s", gitURL, dir)
	}

	envKey := o.Environment
	// lets modify the requirements
	idx := -1

	for i, e := range requirements.Environments {
		if e.Key == envKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		requirements.Environments = append(requirements.Environments, config.EnvironmentConfig{
			Key: envKey,
		})
		idx = len(requirements.Environments) - 1
	}
	requirements.Environments[idx].Owner = cr.Owner
	requirements.Environments[idx].Repository = cr.Repository
	requirements.Environments[idx].RemoteCluster = true

	err = requirements.SaveConfig(fileName)
	if err != nil {
		return errors.Wrapf(err, "failed to save modified requirements file: %s", fileName)
	}

	// TODO do we need to git add?

	commitTitle := fmt.Sprintf("fix: add remote environment %s", envKey)
	commitBody := "adds a link to the new remote environment git repository"
	if o.CreatedScmRepository != nil {
		link := o.CreatedScmRepository.Link
		if link != "" {
			commitBody += " at " + link
		}
	}
	return o.EnvFactory.CreatePullRequest(dir, gitURL, kind, "", commitTitle, commitBody)
}
