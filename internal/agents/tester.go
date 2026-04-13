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

// Tester runs the target repository's detected test command and
// reports pass/fail. By default (v0.3b) it executes on the host with
// the host's toolchain; with `Sandbox=true` (v0.5b) it runs the same
// command inside a Docker container with the repo mounted read-only.
// Sandbox mode is opt-in because it requires an image selection and a
// docker daemon, but it's the safer path when you're about to run
// test suites that came from someone else's patch.
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

	// Sandbox enables Docker-based isolation for test runs. When true,
	// the detected test command is executed inside a container using
	// the Image below, with the repository mounted at /work.
	Sandbox bool

	// Image is the Docker image reference used in sandbox mode. Users
	// pick their own image so the toolchain matches what their project
	// actually needs — we don't ship our own runner image.
	Image string

	// Writable mounts the repo read-write when true. Default (false)
	// mounts read-only so a buggy test can't mutate the working tree
	// from inside the container.
	Writable bool

	// DockerBin is the docker binary path; defaults to "docker".
	// Exported so tests can inject a fake binary.
	DockerBin string
}

// NewTester builds a Tester from the router's RoleTester mapping (the
// small tier by default — failure summaries don't need deep reasoning).
// Sandbox mode is off by default; enable it via SetSandbox().
func NewTester(router *llm.Router) (*Tester, error) {
	p, err := router.For(llm.RoleTester)
	if err != nil {
		return nil, err
	}
	return &Tester{
		Provider:  p,
		Timeout:   5 * time.Minute,
		DockerBin: "docker",
	}, nil
}

// SetSandbox enables Docker isolation for subsequent Run() calls. image
// is required — we don't guess at what runtime the user's project
// needs. writable mounts the repo read-write; the default (false)
// mounts read-only.
func (t *Tester) SetSandbox(image string, writable bool) {
	t.Sandbox = true
	t.Image = image
	t.Writable = writable
}

// TestResult is the output of a single Tester run.
type TestResult struct {
	Command   string
	Detected  string // short label for the detection (go, node, cargo, etc.)
	Sandboxed bool   // true when the command ran inside a Docker container
	ExitCode  int
	Passed    bool
	Output    string // tail of combined stdout+stderr, trimmed to a reasonable size
	Duration  time.Duration
	Summary   string // LLM summary, only populated on failure
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

	// Fail fast on sandbox misconfiguration BEFORE touching the context
	// — a nil ctx would panic in context.WithTimeout later, and we'd
	// rather surface a clean config error than a runtime panic.
	if t.Sandbox && t.Image == "" {
		return nil, errors.New("tester: sandbox enabled but no image set (call SetSandbox first)")
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

	// Rewrite cmdArgs + CWD into a docker invocation when sandboxing
	// is enabled. wrapInDocker is a pure function (tested directly) so
	// we can unit-test the command construction without a real docker
	// daemon.
	var cmd *exec.Cmd
	if t.Sandbox {
		dockerBin := t.DockerBin
		if dockerBin == "" {
			dockerBin = "docker"
		}
		dockerArgs := wrapInDocker(t.Image, repoRoot, t.Writable, cmdArgs)
		cmd = exec.CommandContext(runCtx, dockerBin, dockerArgs...)
		// CWD for docker itself doesn't matter — the container's CWD
		// is set via -w /work inside wrapInDocker.
	} else {
		cmd = exec.CommandContext(runCtx, cmdArgs[0], cmdArgs[1:]...)
		cmd.Dir = repoRoot
	}

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
		Command:   strings.Join(cmdArgs, " "),
		Detected:  label,
		Sandboxed: t.Sandbox,
		ExitCode:  exitCode,
		Passed:    exitCode == 0,
		Output:    output,
		Duration:  duration,
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

// wrapInDocker rewrites a host-native test command into a
// `docker run --rm -v <repo>:/work[:ro] -w /work <image> <cmd...>`
// invocation. Pure function — exported so tests can exercise every
// branch without a real docker daemon.
//
// The container runs with `--rm` so it's cleaned up after the test
// exits. The repository mounts at `/work` inside the container; `-w
// /work` sets that as the working directory. Read-only by default;
// pass writable=true to allow tests that legitimately mutate the
// working tree (generated files, coverage output, etc.).
func wrapInDocker(image, repoRoot string, writable bool, cmd []string) []string {
	args := []string{"run", "--rm"}
	mount := repoRoot + ":/work"
	if !writable {
		mount += ":ro"
	}
	args = append(args, "-v", mount, "-w", "/work", image)
	args = append(args, cmd...)
	return args
}
