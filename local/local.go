package local

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/CircleCI-Public/circleci-cli/api/rest"
	"github.com/CircleCI-Public/circleci-cli/config"
	"github.com/CircleCI-Public/circleci-cli/pipeline"
	"github.com/CircleCI-Public/circleci-cli/settings"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
)

var picardRepo = "circleci/picard"

const DefaultConfigPath = ".circleci/config.yml"

func Execute(flags *pflag.FlagSet, cfg *settings.Config) error {
	var err error
	var configResponse *config.ConfigResponse
	restClient := rest.New(cfg.Host, cfg)
	processedArgs, configPath := buildAgentArguments(flags)

	//if no orgId provided use org slug
	orgID, _ := flags.GetString("org-id")
	if strings.TrimSpace(orgID) != "" {
		configResponse, err = config.ConfigQuery(restClient, configPath, orgID, nil, pipeline.LocalPipelineValues())
		if err != nil {
			return err
		}
	} else {
		orgSlug, _ := flags.GetString("org-slug")
		configResponse, err = config.ConfigQuery(restClient, configPath, orgSlug, nil, pipeline.LocalPipelineValues())
		if err != nil {
			return err
		}
	}

	if !configResponse.Valid {
		return fmt.Errorf("config errors %v", configResponse.Errors)
	}

	processedConfigPath, err := writeStringToTempFile(configResponse.OutputYaml)

	// The file at processedConfigPath must be left in place until after the call
	// to `docker run` has completed. Typically, we would `defer` a call to remove
	// the file. In this case, we execute `docker` using `syscall.Exec`, which
	// replaces the current process, and no more go code will execute at that
	// point, so we cannot delete the file easily. We choose to leave the file
	// in-place in /tmp.

	if err != nil {
		return err
	}

	pwd, err := os.Getwd()

	if err != nil {
		return err
	}

	dockerPath, err := ensureDockerIsAvailable()

	if err != nil {
		return err
	}

	image, err := picardImage(os.Stdout)

	if err != nil {
		return errors.Wrap(err, "Could not find picard image")
	}

	arguments := generateDockerCommand(processedConfigPath, image, pwd, processedArgs...)

	if cfg.Debug {
		_, err = fmt.Fprintf(os.Stderr, "Starting docker with args: %s", arguments)
		if err != nil {
			return err
		}
	}

	if err != nil {
		return errors.Wrap(err, "Could not find a `docker` executable on $PATH; please ensure that docker installed")
	}

	err = syscall.Exec(dockerPath, arguments, os.Environ()) // #nosec
	return errors.Wrap(err, "failed to execute docker")
}

// The `local execute` command proxies execution to the picard docker container,
// and ultimately to `build-agent`. We want to pass most arguments passed to the
// `local execute` command on to build-agent
// These options are here to retain a mock of the flags used by `build-agent`.
// They don't reflect the entire structure or available flags, only those which
// are public in the original command.
func AddFlagsForDocumentation(flags *pflag.FlagSet) {
	flags.StringP("config", "c", DefaultConfigPath, "config file")
	flags.String("job", "build", "job to be executed")
	flags.Int("node-total", 1, "total number of parallel nodes")
	flags.Int("index", 0, "node index of parallelism")
	flags.Bool("skip-checkout", true, "use local path as-is")
	flags.StringArrayP("volume", "v", nil, "Volume bind-mounting")
	flags.String("checkout-key", "~/.ssh/id_rsa", "Git Checkout key")
	flags.String("revision", "", "Git Revision")
	flags.String("branch", "", "Git branch")
	flags.String("repo-url", "", "Git Url")
	flags.StringArrayP("env", "e", nil, "Set environment variables, e.g. `-e VAR=VAL`")
}

// Given the full set of flags that were passed to this command, return the path
// to the config file, and the list of supplied args _except_ for the `--config`
// or `-c` argument, and except for --debug and --org-slug which are consumed by
// this program.
// The `build-agent` can only deal with config version 2.0. In order to feed
// version 2.0 config to it, we need to process the supplied config file using the
// GraphQL API, and feed the result of that into `build-agent`. The first step of
// that process is to find the local path to the config file. This is supplied with
// the `config` flag.
func buildAgentArguments(flags *pflag.FlagSet) ([]string, string) {

	var result []string = []string{}

	// build a list of all supplied flags, that we will pass on to build-agent
	flags.Visit(func(flag *pflag.Flag) {
		if flag.Name != "org-slug" && flag.Name != "config" && flag.Name != "debug" && flag.Name != "org-id" {
			result = append(result, unparseFlag(flags, flag)...)
		}
	})
	result = append(result, flags.Args()...)

	configPath, _ := flags.GetString("config")

	return result, configPath
}

func picardImage(output io.Writer) (string, error) {

	fmt.Fprintf(output, "Fetching latest build environment...\n")
	sha, err := findLatestPicardSha()

	if err != nil {
		return "", err
	}

	_, _ = fmt.Fprintf(output, "Docker image digest: %s\n", sha)
	return fmt.Sprintf("%s@%s", picardRepo, sha), nil
}

func ensureDockerIsAvailable() (string, error) {

	dockerPath, err := exec.LookPath("docker")

	if err != nil {
		return "", errors.New("could not find `docker` on the PATH; please ensure that docker is installed")
	}

	dockerRunning := exec.Command(dockerPath, "version").Run() == nil // #nosec

	if !dockerRunning {
		return "", errors.New("failed to connect to docker; please ensure that docker is running, and that `docker version` succeeds")
	}

	return dockerPath, nil
}

// Still depends on a function in cmd/build.go
func findLatestPicardSha() (string, error) {

	if _, err := ensureDockerIsAvailable(); err != nil {
		return "", err
	}

	outputBytes, err := exec.Command("docker", "pull", picardRepo).CombinedOutput() // #nosec

	if err != nil {
		return "", errors.Wrap(err, "failed to pull latest docker image")
	}

	output := string(outputBytes)
	sha256 := regexp.MustCompile("(?m)sha256:[0-9a-f]+")
	latest := sha256.FindString(output)

	if latest == "" {
		return "", fmt.Errorf("failed to parse sha256 from docker pull output")
	}

	return latest, nil
}

// Write data to a temp file, and return the path to that file.
func writeStringToTempFile(data string) (string, error) {
	// It's important to specify `/tmp` here as the location of the temp file.
	// On macOS, the regular temp directories is not shared with Docker by default.
	// The error message is along the lines of:
	// > The path /var/folders/q0/2g2lcf6j79df6vxqm0cg_0zm0000gn/T/287575618-config.yml
	// > is not shared from OS X and is not known to Docker.
	// Docker has `/tmp` shared by default.
	f, err := ioutil.TempFile("/tmp", "*_circleci_config.yml")

	if err != nil {
		return "", errors.Wrap(err, "Error creating temporary config file")
	}

	if _, err = f.WriteString(data); err != nil {
		return "", errors.Wrap(err, "Error writing processed config to temporary file")
	}

	return f.Name(), nil
}

func generateDockerCommand(configPath, image, pwd string, arguments ...string) []string {
	const configPathInsideContainer = "/tmp/local_build_config.yml"
	core := []string{"docker", "run", "--interactive", "--tty", "--rm",
		"--volume", "/var/run/docker.sock:/var/run/docker.sock",
		"--volume", fmt.Sprintf("%s:%s", configPath, configPathInsideContainer),
		"--volume", fmt.Sprintf("%s:%s", pwd, pwd),
		"--volume", fmt.Sprintf("%s:/root/.circleci", settings.SettingsPath()),
		"--workdir", pwd,
		image, "circleci", "build", "--config", configPathInsideContainer}
	return append(core, arguments...)
}

// Convert the given flag back into a list of strings suitable to be passed on
// the command line to run docker.
// https://github.com/CircleCI-Public/circleci-cli/issues/391
func unparseFlag(flags *pflag.FlagSet, flag *pflag.Flag) []string {
	flagName := "--" + flag.Name
	result := []string{}
	switch flag.Value.Type() {
	// A stringArray type argument is collapsed into a single flag:
	// `--foo 1 --foo 2` will result in a single `foo` flag with an array of values.
	case "stringArray":
		for _, value := range flag.Value.(pflag.SliceValue).GetSlice() {
			result = append(result, flagName, value)
		}
	default:
		result = append(result, flagName, flag.Value.String())
	}
	return result
}
