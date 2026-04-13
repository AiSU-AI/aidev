package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Tester is the v0.3b agent that runs the target repository's detected
// test command and reports pass/fail. It never runs in a sandbox — tests
// execute against the real working tree with the real process
// environment, because aidev's safety story is "we propose, the human
// applies, the tests run where the human ran git apply".
//
// On test failure the Tester asks the medium-tier LLM for a 3-sentence
// summary of what broke, which is attached to the TestResult. On success
// the LLM is skipped entirely to save cost.
type Tester struct {
	// Provider is the medium-tier model used for failure summaries.
	Provider llm.Provider

	// Timeout is the wall-clock limit for the test command. Zero means
	// the default (5 minutes).
	Timeout time.Duration
}

// NewTester builds a Tester from the router's RoleTester mapping (the
// small tier by default — failure summaries don't need deep reasoning).
func NewTester(router *llm.Router) (*Tester, error) {
	p, err := router.For(llm.RoleTester)
	if err != nil {
		return nil, err
	}
	return &Tester{Provider: p, Timeout: 5 * time.Minute}, nil
}

// TestResult is the output of a single Tester run.
type TestResult struct {
	Command  string
	Detected string        // short label for the detection (go, node, cargo, etc.)
	ExitCode int
	Passed   bool
	Output   string // tail of combined stdout+stderr, trimmed to a reasonable size
	Duration time.Duration
	Summary  string // LLM summary, only populated on failure
}

// DetectTestCommand inspects the top-level files of repoRoot and returns
// a (command, label, err) triple describing how to run the project's
// test suite. Returns an error when no known project type is detected;
// the caller can either fail loudly or prompt the user for a custom
// command.
func DetectTestCommand(repoRoot string) (cmd []string, label string, err error) {
	if repoRoot == "" {
		return nil, "", errors.New("tester: empty repo root")
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(repoRoot, rel))
		return err == nil
	}

	// Priority order: more specific project types first. A Makefile can
	// live in almost any project so we check it last among the "always
	// works" options.
	switch {
	case exists("go.mod"):
		return []string{"go", "test", "./..."}, "go", nil
	case exists("Cargo.toml"):
		return []string{"cargo", "test"}, "cargo", nil
	case exists("pyproject.toml") || exists("setup.py") || exists("requirements.txt"):
		return []string{"pytest"}, "python", nil
	case exists("package.json"):
		// Default to `npm test`. Users with pnpm/yarn can override via
		// the TESTER_COMMAND env var (see Run).
		return []string{"npm", "test"}, "node", nil
	case exists("build.gradle") || exists("build.gradle.kts"):
		return []string{"./gradlew", "test"}, "gradle", nil
	case exists("pom.xml"):
		return []string{"mvn", "test"}, "maven", nil
	case exists("justfile"):
		return []string{"just", "test"}, "just", nil
	case exists("Makefile"):
		// Only use make test if the Makefile actually has a 'test'
		// target — otherwise `make test` hangs on the default rule.
		if makefileHasTarget(filepath.Join(repoRoot, "Makefile"), "test") {
			return []string{"make", "test"}, "make", nil
		}
	}
	return nil, "", fmt.Errorf("tester: no known test runner detected in %s", repoRoot)
}

// makefileHasTarget is a very cheap Makefile scanner — it looks for a
// line starting with "target:" at the left margin. Good enough to tell
// "make test" apart from "make" on a Makefile that has no test target.
func makefileHasTarget(path, target string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	prefix := target + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// Run executes the detected test command and returns a TestResult. The
// command runs with the repo as CWD and inherits aidev's environment,
// so whatever shell aliases / PATH / language version managers the
// user has in their own terminal will apply.
//
// Environment overrides:
//
//   - AIDEV_TEST_COMMAND — if set, this exact command (split by spaces)
//     replaces the detected one. Used for setups that pnpm/yarn/poetry
//     instead of npm/pip.
func (t *Tester) Run(ctx context.Context, repoRoot string) (*TestResult, error) {
	if repoRoot == "" {
		return nil, errors.New("tester: empty repo root")
	}

	var cmdArgs []string
	label := ""
	if override := os.Getenv("AIDEV_TEST_COMMAND"); override != "" {
		cmdArgs = strings.Fields(override)
		label = "override"
	} else {
		var err error
		cmdArgs, label, err = DetectTestCommand(repoRoot)
		if err != nil {
			return nil, err
		}
	}
	if len(cmdArgs) == 0 {
		return nil, errors.New("tester: empty command")
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = repoRoot
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1 // tooling failure (command not found, timeout)
		}
	}

	output := tailString(buf.String(), 4096)
	result := &TestResult{
		Command:  strings.Join(cmdArgs, " "),
		Detected: label,
		ExitCode: exitCode,
		Passed:   exitCode == 0,
		Output:   output,
		Duration: duration,
	}

	if !result.Passed {
		summary, serr := t.summariseFailure(ctx, result)
		if serr != nil {
			// Failure summary is best-effort; don't let the LLM call
			// mask the real test failure.
			result.Summary = "(failure summary unavailable: " + serr.Error() + ")"
		} else {
			result.Summary = summary
		}
	}
	return result, nil
}

// summariseFailure asks the small tier for a 3-sentence description of
// what failed. The prompt is deliberately tight — we want an artefact
// the user can skim, not a long essay.
func (t *Tester) summariseFailure(ctx context.Context, r *TestResult) (string, error) {
	if t.Provider == nil {
		return "", errors.New("no provider configured")
	}
	system := "You are a test failure summariser. Read the output of a failing " +
		"test run and produce EXACTLY three sentences: (1) what failed, (2) which " +
		"test(s) or package(s) are affected, (3) the most likely root cause based " +
		"on the message. Do not speculate beyond the evidence. Do not wrap in a " +
		"code block."
	user := "Test command: " + r.Command + "\n" +
		"Exit code: " + fmt.Sprint(r.ExitCode) + "\n\n" +
		"Output (tail):\n\n" + r.Output
	resp, err := t.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// tailString returns the last n characters of s. Used to keep the
// captured test output to a reasonable size when tests produce
// megabytes of logs. If s is shorter than n, it is returned unchanged.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...[truncated]...\n" + s[len(s)-n:]
}
