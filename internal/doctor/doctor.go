// Package doctor runs a precondition audit of aidev's environment: config
// validity, required secrets, local model daemon health, and required
// models. It powers two entry points:
//
//   - `aidev doctor` — prints a full report and exits; never prompts.
//   - automatic precondition runs on normal `aidev -issue ...` startup,
//     which can prompt for interactive fixes (spawn Ollama, pull a model,
//     swap to an alternative) when stdin is a TTY.
//
// The package is deliberately separated from the orchestrator so that doctor
// can be invoked with only a config, without building the full agent graph.
package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/config"
)

// Severity classifies a check result. OK passes silently in auto mode;
// WARN is printed but not blocking; FAIL blocks the pipeline.
type Severity int

const (
	OK Severity = iota
	WARN
	FAIL
)

// String gives the human label for a severity.
func (s Severity) String() string {
	switch s {
	case OK:
		return "OK"
	case WARN:
		return "WARN"
	case FAIL:
		return "FAIL"
	}
	return "?"
}

// Result is a single check's outcome.
type Result struct {
	Name        string   // short identifier: "config", "ollama-daemon", "anthropic-key"
	Severity    Severity
	Message     string   // one-line summary shown in the report
	Remediation string   // what the user can do to fix this (empty for OK)
	Details     []string // optional extra lines printed under the summary
}

// Format renders a Result as a single multi-line block suitable for a TTY
// report. Format is pure so it can be unit-tested.
func (r Result) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s — %s\n", r.Severity, r.Name, r.Message)
	for _, d := range r.Details {
		fmt.Fprintf(&b, "      %s\n", d)
	}
	if r.Remediation != "" && r.Severity != OK {
		fmt.Fprintf(&b, "      fix: %s\n", r.Remediation)
	}
	return b.String()
}

// Report is the outcome of a full diagnostic run.
type Report struct {
	Results []Result
}

// Any returns true if any result matches the given severity.
func (r Report) Any(sev Severity) bool {
	for _, res := range r.Results {
		if res.Severity == sev {
			return true
		}
	}
	return false
}

// HasFailure is a convenience for callers that only care whether the
// pipeline should abort.
func (r Report) HasFailure() bool { return r.Any(FAIL) }

// Write renders the whole report to w with a trailing summary line.
func (r Report) Write(w io.Writer) {
	for _, res := range r.Results {
		fmt.Fprint(w, res.Format())
	}
	ok, warn, fail := 0, 0, 0
	for _, res := range r.Results {
		switch res.Severity {
		case OK:
			ok++
		case WARN:
			warn++
		case FAIL:
			fail++
		}
	}
	fmt.Fprintf(w, "\nsummary: %d ok, %d warn, %d fail\n", ok, warn, fail)
}

// Options configures a doctor run.
type Options struct {
	// Interactive enables prompts for fixable problems. When false, the
	// doctor only reports — it never blocks waiting for input. Set to
	// false in CI, headless, and `aidev doctor` invocations; set to true
	// in normal interactive startup.
	Interactive bool

	// AutoSpawnOllama enables the "start the daemon if the binary exists
	// but the daemon is unreachable" behaviour. Harmless when false; the
	// check just reports the daemon as unreachable.
	AutoSpawnOllama bool

	// Prompter is the function used to read user input during interactive
	// fixes. Exposed as a field so tests can inject a deterministic
	// answerer. Defaults to a stdin reader when nil.
	Prompter Prompter

	// Out is where the doctor writes progress/prompt output. Defaults to
	// os.Stderr when nil.
	Out io.Writer
}

// Prompter reads a single line of user input after writing a prompt. A
// nil Prompter in Options is replaced at Run time with a stdin-backed
// default.
type Prompter func(prompt string) (string, error)

// Run executes all checks against cfg and returns the resulting Report.
// When Opts.Interactive is true, fixable problems can be resolved inline
// (e.g. spawning Ollama, pulling a model, swapping to an alternative) and
// the corresponding Results are updated in place before Run returns.
func Run(ctx context.Context, cfg *config.Config, opts Options) Report {
	if opts.Out == nil {
		opts.Out = discardWriter{}
	}
	if opts.Prompter == nil {
		opts.Prompter = stdinPrompter
	}

	report := Report{}

	// Order matters: config first because everything downstream depends
	// on it. Then the cheap checks (filesystem, env vars). Then the
	// network-touching checks (Ollama daemon, model list). Finally the
	// interactive fixes, which need all the prior checks to know what's
	// missing.
	report.Results = append(report.Results, checkConfig(cfg))
	report.Results = append(report.Results, checkAnthropicKey(cfg))

	ollamaCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ollamaResults := checkOllama(ollamaCtx, cfg, opts)
	report.Results = append(report.Results, ollamaResults...)

	return report
}

// discardWriter is an io.Writer that swallows everything. Used as the
// default Opts.Out so callers that don't want progress output don't have
// to allocate anything.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
