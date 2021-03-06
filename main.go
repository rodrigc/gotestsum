package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"gotest.tools/gotestsum/testjson"
)

func main() {
	name := os.Args[0]
	flags, opts := setupFlags(name)
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}
	opts.args = flags.Args()
	setupLogging(opts)

	switch err := run(opts).(type) {
	case nil:
	case *exec.ExitError:
		// go test should already report the error to stderr so just exit with
		// the same status code
		os.Exit(ExitCodeWithDefault(err))
	default:
		fmt.Fprintln(os.Stderr, name+": Error: "+err.Error())
		os.Exit(3)
	}
}

func setupFlags(name string) (*pflag.FlagSet, *options) {
	opts := &options{}
	flags := pflag.NewFlagSet(name, pflag.ContinueOnError)
	flags.SetInterspersed(false)
	flags.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage:
    %s [flags] [--] [go test flags]

Flags:
`, name)
		flags.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Formats:
    dots              print a character for each test
    short             print a line for each package
    short-verbose     print a line for each test and package
    standard-quiet    default go test format
    standard-verbose  default go test -v format
`)
	}
	flags.BoolVar(&opts.debug, "debug", false, "enabled debug")
	flags.StringVar(&opts.format, "format",
		lookEnvWithDefault("GOTESTSUM_FORMAT", "short"),
		"print format of test input")
	flags.BoolVar(&opts.rawCommand, "raw-command", false,
		"don't prepend 'go test -json' to the 'go test' command")
	flags.StringVar(&opts.jsonFile, "jsonfile",
		lookEnvWithDefault("GOTESTSUM_JSONFILE", ""),
		"write all TestEvents to file")
	flags.StringVar(&opts.junitFile, "junitfile",
		lookEnvWithDefault("GOTESTSUM_JUNITFILE", ""),
		"write a JUnit XML file")
	flags.BoolVar(&opts.noColor, "no-color", false, "disable color output")
	flags.StringSliceVar(&opts.noSummary, "no-summary", nil,
		"do not print summary of: failed, skipped, errors")
	return flags, opts
}

func lookEnvWithDefault(key, defValue string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return defValue
}

type options struct {
	args       []string
	format     string
	debug      bool
	rawCommand bool
	jsonFile   string
	junitFile  string
	noColor    bool
	noSummary  []string
}

func setupLogging(opts *options) {
	if opts.debug {
		log.SetLevel(log.DebugLevel)
	}
	if opts.noColor {
		color.NoColor = true
	}
}

// TODO: add flag --max-failures
func run(opts *options) error {
	ctx := context.Background()
	goTestProc, err := startGoTest(ctx, goTestCmdArgs(opts))
	if err != nil {
		return errors.Wrapf(err, "failed to run %s %s",
			goTestProc.cmd.Path,
			strings.Join(goTestProc.cmd.Args, " "))
	}
	defer goTestProc.cancel()

	out := os.Stdout
	handler, err := newEventHandler(opts, out, os.Stderr)
	if err != nil {
		return err
	}
	defer handler.Close() // nolint: errcheck
	exec, err := testjson.ScanTestOutput(testjson.ScanConfig{
		Stdout:  goTestProc.stdout,
		Stderr:  goTestProc.stderr,
		Handler: handler,
	})
	if err != nil {
		return err
	}
	if err := summarizer(opts)(out, exec); err != nil {
		return err
	}
	if err := writeJUnitFile(opts.junitFile, exec); err != nil {
		return err
	}
	return goTestProc.cmd.Wait()
}

func goTestCmdArgs(opts *options) []string {
	args := opts.args
	defaultArgs := []string{"go", "test"}
	switch {
	case opts.rawCommand:
		return args
	case len(args) == 0:
		return append(defaultArgs, "-json", pathFromEnv("./..."))
	case !hasJSONArg(args):
		defaultArgs = append(defaultArgs, "-json")
	}
	if testPath := pathFromEnv(""); testPath != "" {
		args = append(args, testPath)
	}
	return append(defaultArgs, args...)
}

func pathFromEnv(defaultPath string) string {
	return lookEnvWithDefault("TEST_DIRECTORY", defaultPath)
}

func hasJSONArg(args []string) bool {
	for _, arg := range args {
		if arg == "-json" || arg == "--json" {
			return true
		}
	}
	return false
}

type proc struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr io.Reader
	cancel func()
}

func startGoTest(ctx context.Context, args []string) (proc, error) {
	ctx, cancel := context.WithCancel(ctx)
	p := proc{
		cmd:    exec.CommandContext(ctx, args[0], args[1:]...),
		cancel: cancel,
	}
	log.Debugf("exec: %s", p.cmd.Args)
	var err error
	p.stdout, err = p.cmd.StdoutPipe()
	if err != nil {
		return p, err
	}
	p.stderr, err = p.cmd.StderrPipe()
	if err != nil {
		return p, err
	}
	err = p.cmd.Start()
	if err == nil {
		log.Debugf("go test pid: %d", p.cmd.Process.Pid)
	}
	return p, err
}

func summarizer(opts *options) func(io.Writer, *testjson.Execution) error {
	summary := testjson.SummarizeAll
	// TODO: do this in a pflag.Value to validate the string
	for _, item := range opts.noSummary {
		switch item {
		case "failed":
			summary -= testjson.SummarizeFailed
		case "skipped":
			summary -= testjson.SummarizeSkipped
		case "errors":
			summary -= testjson.SummarizeErrors
		}
	}
	return func(out io.Writer, exec *testjson.Execution) error {
		return testjson.PrintSummary(out, exec, summary)
	}
}
