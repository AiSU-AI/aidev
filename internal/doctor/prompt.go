package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
)

// userChoice is the outcome of a prompt for a missing model: pull it,
// swap to an existing alternative, or abort.
type userChoice int

const (
	pullIt userChoice = iota
	swapToInstalled
	abort
)

// IsTTY reports whether stdin and stdout both look like TTYs. Doctor uses
// this to decide whether it can prompt the user. Exported so main.go can
// consult it when deciding whether to enable Options.Interactive.
func IsTTY() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// stdinPrompter is the default Prompter used when Options.Prompter is nil.
// It prints the prompt to stderr (to avoid tainting stdout in headless
// modes that pipe the report) and reads a line from stdin.
func stdinPrompter(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptMissingModel runs the interactive "missing model" dialogue for a
// single required model. It returns the user's choice, the alternative
// model name (when choice == swapToInstalled), and any error.
//
// The menu offers three options:
//
//	[1] Pull <want> now  — continue with the declared routing, download the model
//	[2] Swap to an existing model (only shown if installed has entries)
//	[3] Abort — work cannot begin
//
// The output is intentionally plain text so it renders fine in any TTY.
func promptMissingModel(opts Options, want string, installed []string) (userChoice, string, error) {
	// Filter to installed models that look like plausible alternatives.
	// For now that's "any installed model" — we don't try to be clever
	// about coder-vs-generalist models; aidev's router doesn't know the
	// difference either.
	alternatives := make([]string, 0, len(installed))
	for _, m := range installed {
		if m == want {
			continue
		}
		alternatives = append(alternatives, m)
	}

	var menu strings.Builder
	fmt.Fprintf(&menu, "\nOllama model %q is required but not installed.\n\n", want)
	fmt.Fprintf(&menu, "  [1] Pull %s now (multi-GB download, requires network)\n", want)
	if len(alternatives) > 0 {
		fmt.Fprintf(&menu, "  [2] Swap to one of your already-installed models\n")
		fmt.Fprintf(&menu, "  [3] Abort — aidev cannot run without a model for this tier\n")
	} else {
		fmt.Fprintf(&menu, "  [3] Abort — no local alternatives installed, aidev cannot run\n")
	}
	menu.WriteString("\nChoice [1")
	if len(alternatives) > 0 {
		menu.WriteString("/2")
	}
	menu.WriteString("/3]: ")

	answer, err := opts.Prompter(menu.String())
	if err != nil {
		return abort, "", fmt.Errorf("read choice: %w", err)
	}
	switch strings.TrimSpace(answer) {
	case "1":
		return pullIt, "", nil
	case "2":
		if len(alternatives) == 0 {
			return abort, "", errors.New("option 2 is not available (no local alternatives)")
		}
		return promptPickAlternative(opts, alternatives)
	case "3", "":
		return abort, "", nil
	default:
		return abort, "", fmt.Errorf("unrecognised choice %q", answer)
	}
}

// promptPickAlternative asks the user to pick one of the installed models
// by number. Returns the chosen name as the swap target.
func promptPickAlternative(opts Options, installed []string) (userChoice, string, error) {
	var menu strings.Builder
	menu.WriteString("\nInstalled models:\n")
	for i, m := range installed {
		fmt.Fprintf(&menu, "  [%d] %s\n", i+1, m)
	}
	fmt.Fprintf(&menu, "\nPick one [1-%d]: ", len(installed))
	answer, err := opts.Prompter(menu.String())
	if err != nil {
		return abort, "", fmt.Errorf("read pick: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(installed) {
		return abort, "", fmt.Errorf("invalid pick %q", answer)
	}
	return swapToInstalled, installed[n-1], nil
}
