package cmd

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"strings"

	"github.com/CircleCI-Public/circleci-cli/api/rest"
	"github.com/CircleCI-Public/circleci-cli/config"
	"github.com/CircleCI-Public/circleci-cli/filetree"
	"github.com/CircleCI-Public/circleci-cli/local"
	"github.com/CircleCI-Public/circleci-cli/pipeline"
	"github.com/CircleCI-Public/circleci-cli/proxy"
	"github.com/CircleCI-Public/circleci-cli/settings"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

type configOptions struct {
	cfg  *settings.Config
	rest *rest.Client
	args []string
}

// Path to the config.yml file to operate on.
// Used to for compatibility with `circleci config validate --path`
var configPath string
var ignoreDeprecatedImages bool // should we ignore deprecated images warning

var configAnnotations = map[string]string{
	"<path>": "The path to your config (use \"-\" for STDIN)",
}

func newConfigCommand(config *settings.Config) *cobra.Command {
	opts := configOptions{
		cfg: config,
	}

	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Operate on build config files",
	}

	packCommand := &cobra.Command{
		Use:   "pack <path>",
		Short: "Pack up your CircleCI configuration into a single file.",
		PreRun: func(cmd *cobra.Command, args []string) {
			opts.args = args
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return packConfig(opts)
		},
		Args:        cobra.ExactArgs(1),
		Annotations: make(map[string]string),
	}
	packCommand.Annotations["<path>"] = configAnnotations["<path>"]

	validateCommand := &cobra.Command{
		Use:     "validate <path>",
		Aliases: []string{"check"},
		Short:   "Check that the config file is well formed.",
		PreRun: func(cmd *cobra.Command, args []string) {
			opts.args = args
			opts.rest = rest.New(config.Host, config)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return validateConfig(opts, cmd.Flags())
		},
		Args:        cobra.MaximumNArgs(1),
		Annotations: make(map[string]string),
	}
	validateCommand.Annotations["<path>"] = configAnnotations["<path>"]
	validateCommand.PersistentFlags().StringVarP(&configPath, "config", "c", ".circleci/config.yml", "path to config file")
	validateCommand.PersistentFlags().BoolVar(&ignoreDeprecatedImages, "ignore-deprecated-images", false, "ignores the deprecated images error")
	if err := validateCommand.PersistentFlags().MarkHidden("config"); err != nil {
		panic(err)
	}
	validateCommand.Flags().StringP("org-slug", "o", "", "organization slug (for example: github/example-org), used when a config depends on private orbs belonging to that org")
	validateCommand.Flags().String("org-id", "", "organization id used when a config depends on private orbs belonging to that org")

	processCommand := &cobra.Command{
		Use:   "process <path>",
		Short: "Validate config and display expanded configuration.",
		PreRun: func(cmd *cobra.Command, args []string) {
			opts.args = args
			opts.rest = rest.New(config.Host, config)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return processConfig(opts, cmd.Flags())
		},
		Args:        cobra.ExactArgs(1),
		Annotations: make(map[string]string),
	}
	processCommand.Annotations["<path>"] = configAnnotations["<path>"]
	processCommand.Flags().StringP("org-slug", "o", "", "organization slug (for example: github/example-org), used when a config depends on private orbs belonging to that org")
	processCommand.Flags().String("org-id", "", "organization id used when a config depends on private orbs belonging to that org")
	processCommand.Flags().StringP("pipeline-parameters", "", "", "YAML/JSON map of pipeline parameters, accepts either YAML/JSON directly or file path (for example: my-params.yml)")

	migrateCommand := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a pre-release 2.0 config to the official release version",
		PreRun: func(cmd *cobra.Command, args []string) {
			opts.args = args
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return migrateConfig(opts)
		},
		Hidden:             true,
		DisableFlagParsing: true,
	}
	// These flags are for documentation and not actually parsed
	migrateCommand.PersistentFlags().StringP("config", "c", ".circleci/config.yml", "path to config file")
	migrateCommand.PersistentFlags().BoolP("in-place", "i", false, "whether to update file in place.  If false, emits to stdout")

	configCmd.AddCommand(packCommand)
	configCmd.AddCommand(validateCommand)
	configCmd.AddCommand(processCommand)
	configCmd.AddCommand(migrateCommand)

	return configCmd
}

// The <path> arg is actually optional, in order to support compatibility with the --path flag.
func validateConfig(opts configOptions, flags *pflag.FlagSet) error {
	var err error
	var response *config.ConfigResponse
	path := local.DefaultConfigPath
	// First, set the path to configPath set by --path flag for compatibility
	if configPath != "" {
		path = configPath
	}

	// Then, if an arg is passed in, choose that instead
	if len(opts.args) == 1 {
		path = opts.args[0]
	}

	//if no orgId provided use org slug
	var orgID string
	orgID, _ = flags.GetString("org-id")
	if strings.TrimSpace(orgID) != "" {
		orgID, _ = flags.GetString("org-id")
		response, err = config.ConfigQuery(opts.rest, path, orgID, nil, pipeline.LocalPipelineValues())
		if err != nil {
			return err
		}
	} else {
		orgSlug, _ := flags.GetString("org-slug")
		orgs, err := GetOrgCollaborations(opts.rest)
		if err != nil {
			fmt.Println(err.Error())
		}
		orgID = GetOrgIdFromSlug(orgSlug, orgs)
		response, err = config.ConfigQuery(opts.rest, path, orgID, nil, pipeline.LocalPipelineValues())
		if err != nil {
			return err
		}
	}

	// check if a deprecated Linux VM image is being used
	// link here to blog post when available
	// returns an error if a deprecated image is used
	if !ignoreDeprecatedImages {
		err := config.DeprecatedImageCheck(response)
		if err != nil {
			return err
		}
	}

	if path == "-" {
		fmt.Printf("Config input is valid.\n")
	} else {
		fmt.Printf("Config file at %s is valid.\n", path)
	}

	return nil
}

func processConfig(opts configOptions, flags *pflag.FlagSet) error {
	paramsYaml, _ := flags.GetString("pipeline-parameters")
	var response *config.ConfigResponse
	var params pipeline.Parameters
	var err error

	if len(paramsYaml) > 0 {
		// The 'src' value can be a filepath, or a yaml string. If the file cannot be read successfully,
		// proceed with the assumption that the value is already valid yaml.
		raw, err := ioutil.ReadFile(paramsYaml)
		if err != nil {
			raw = []byte(paramsYaml)
		}

		err = yaml.Unmarshal(raw, &params)
		if err != nil {
			return fmt.Errorf("invalid 'pipeline-parameters' provided: %s", err.Error())
		}
	}

	//if no orgId provided use org slug
	orgID, _ := flags.GetString("org-id")
	if strings.TrimSpace(orgID) != "" {
		response, err = config.ConfigQuery(opts.rest, opts.args[0], orgID, params, pipeline.LocalPipelineValues())
		if err != nil {
			return err
		}
	} else {
		orgSlug, _ := flags.GetString("org-slug")
		orgs, err := GetOrgCollaborations(opts.rest)
		if err != nil {
			fmt.Println(err.Error())
		}
		orgID = GetOrgIdFromSlug(orgSlug, orgs)
		response, err = config.ConfigQuery(opts.rest, opts.args[0], orgID, params, pipeline.LocalPipelineValues())
		if err != nil {
			return err
		}
	}

	fmt.Print(response.OutputYaml)
	return nil
}

func packConfig(opts configOptions) error {
	tree, err := filetree.NewTree(opts.args[0])
	if err != nil {
		return errors.Wrap(err, "An error occurred trying to build the tree")
	}

	y, err := yaml.Marshal(&tree)
	if err != nil {
		return errors.Wrap(err, "Failed trying to marshal the tree to YAML ")
	}
	fmt.Printf("%s\n", string(y))
	return nil
}

func migrateConfig(opts configOptions) error {
	return proxy.Exec([]string{"config", "migrate"}, opts.args)
}

type CollaborationResult struct {
	VcsTye    string `json:"vcs_type"`
	OrgSlug   string `json:"slug"`
	OrgName   string `json:"name"`
	OrgId     string `json:"id"`
	AvatarUrl string `json:"avatar_url"`
}

// GetOrgCollaborations - fetches all the collaborations for a given user.
func GetOrgCollaborations(client *rest.Client) ([]CollaborationResult, error) {
	req, err := client.NewRequest("GET", &url.URL{Path: "me/collaborations"}, nil)
	if err != nil {
		return nil, err
	}

	var resp []CollaborationResult
	_, err = client.DoRequest(req, &resp)
	return resp, err
}

// GetOrgIdFromSlug - converts a slug into an orgID.
func GetOrgIdFromSlug(slug string, collaborations []CollaborationResult) string {
	for _, v := range collaborations {
		if v.OrgSlug == slug {
			return v.OrgId
		}
	}
	return ""
}
