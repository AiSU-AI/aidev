// Implementer's native-tool-use code path (v0.4).
//
// This file is what Implementer.Run() dispatches to when the
// configured provider implements llm.ToolAwareProvider. It replaces
// the legacy picker + NEED_FILES + harvest + missing-paths tracking
// path with a single agentic loop where the model calls read_file,
// glob, grep, and list_dir as real tools within one LLM session.
//
// The whole file is scoped to this one responsibility. The legacy
// path in implementer.go stays intact as a fallback for providers
// that don't support tool use (Ollama, claude-cli), so users on the
// --preset local path still get a working Implementer — slower and
// less reliable, but functional.
package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// maxToolIterations caps the number of Implementer <-> tool-result
// round-trips per Run() call. The budget matters because each
// iteration is a full LLM round-trip, and a runaway loop (model
// keeps calling tools indefinitely) would balloon cost. 25 is
// generous enough for the real-world consolidation tasks I've seen
// — picker-plus-diff in the legacy path was typically 3-5 LLM
// calls, and the native tool-use path trades fewer calls per
// round for more rounds (read file, grep for refs, read another,
// etc.). 25 gives the model enough headroom to explore a
// multi-locale + multi-component monorepo without tearing through
// the budget.
const maxToolIterations = 25

// maxToolFormatCorrections is the cap on how many times
// runWithTools will send a corrective follow-up when the model
// ends its turn with invalid final content (not a diff, not an
// ERROR: line). One correction is enough for cloud Claude; local
// Ollama models occasionally need two but anything beyond that
// is a model that simply cannot follow the output format and
// deserves a hard failure with a clear error.
const maxToolFormatCorrections = 2

// runWithTools drives the native tool-use loop. Called from
// Implementer.Run() when the provider implements ToolAwareProvider.
//
// The loop shape:
//
//  1. Build initial system prompt + user message + tool definitions.
//  2. Call provider.CompleteWithTools.
//  3. If StopReason is "end_turn", validate the model's final text
//     as a unified diff and return.
//  4. If StopReason is "tool_use", execute every ToolUse via the
//     executor, append the assistant's message AND a new user
//     message containing the tool_result blocks to the running
//     history, and loop.
//  5. Bounded at maxToolIterations to prevent runaway cost.
//
// Unlike the legacy path, there is NO picker turn, NO NEED_FILES
// protocol, NO harvest step, and NO missing-path tracking. The
// model has direct access to the filesystem via tools and the
// conversation state is the only thing the loop has to manage.
//
// Format correction (v0.5a): the loop DOES include one layer of
// format correction, because even tool-capable local models
// (qwen2.5-coder:14b especially) sometimes produce an end_turn
// response whose content isn't a valid diff. The provider layer
// now salvages content-embedded tool calls before we get here,
// so the cases that reach this loop are genuine format misses —
// the model thought it was done but emitted the wrong shape. We
// send one corrective follow-up asking for either a valid diff
// or a real tool_use, and only fail hard if it's wrong twice.
func (i *Implementer) runWithTools(ctx context.Context, provider llm.ToolAwareProvider, c *Context, chosen *Sketch) (*Patch, error) {
	exec := NewToolExecutor(c.Snapshot.Root)
	toolDefs := exec.Definitions()

	system := toolUseSystemPrompt()
	userMsg := buildToolUseUserMessage(c, chosen)

	// Seed the conversation with one user message. The loop appends
	// an assistant message (and a follow-up user tool_result message)
	// on every tool-use iteration.
	messages := []llm.ToolMessage{
		{
			Role: "user",
			Content: []llm.ToolContentBlock{
				{Type: "text", Text: userMsg},
			},
		},
	}

	// formatCorrectionsUsed tracks how many times we've sent a
	// corrective follow-up to recover from a malformed final
	// turn (an end_turn response whose content isn't a diff).
	// Capped at maxToolFormatCorrections so a model that
	// refuses to comply can't loop forever.
	formatCorrectionsUsed := 0

	for iter := 0; iter < maxToolIterations; iter++ {
		resp, err := provider.CompleteWithTools(ctx, llm.ToolAwareRequest{
			System:   system,
			Tools:    toolDefs,
			Messages: messages,
		})
		if err != nil {
			return nil, fmt.Errorf("implementer: tool-use call (iteration %d): %w", iter, err)
		}

		// Append the assistant's turn to history regardless of stop
		// reason. The provider returns AssistantMessage specifically
		// so the caller can round-trip it.
		messages = append(messages, resp.AssistantMessage)

		switch resp.StopReason {
		case "end_turn":
			// Model is done iterating. Its final text is supposed
			// to be a unified diff. If validation passes, return
			// the patch.
			patch, err := validateDiffResponse(resp.Content)
			if err == nil {
				return patch, nil
			}
			// ERROR: is an intentional terminal state — the
			// model is saying the task is impossible. Propagate
			// immediately instead of trying to format-correct it
			// into a diff.
			if errors.Is(err, errDeclaredError) {
				return nil, err
			}
			// Otherwise it's a format miss. Try ONE format
			// correction before failing hard.
			if formatCorrectionsUsed >= maxToolFormatCorrections {
				// Dump the raw output so the user can see exactly
				// what the model produced. This is the debugging
				// signal v0.4a missed — we'd just see "first
				// line was X" and have to guess at the rest.
				return nil, fmt.Errorf("implementer: end_turn output is not a valid diff after %d format corrections: %w\n\nraw output:\n%s",
					formatCorrectionsUsed, err, truncateForError(resp.Content, 2048))
			}
			// Stage a corrective user message that quotes the
			// broken output and tells the model what to do.
			messages = append(messages, llm.ToolMessage{
				Role: "user",
				Content: []llm.ToolContentBlock{
					{
						Type: "text",
						Text: buildFormatCorrectionMessage(resp.Content, err),
					},
				},
			})
			formatCorrectionsUsed++
			// Fall through to the next loop iteration, which
			// re-calls the provider with the corrective message
			// appended.
			continue

		case "tool_use":
			if len(resp.ToolUses) == 0 {
				return nil, errors.New("implementer: stop_reason=tool_use but no tool_use blocks in response")
			}
			// Execute every pending tool call and build the
			// follow-up user message with tool_result blocks.
			resultBlocks := make([]llm.ToolContentBlock, 0, len(resp.ToolUses))
			for _, use := range resp.ToolUses {
				result := exec.Execute(use)
				// Breadcrumb each tool call to stderr so the run log shows
				// exactly what the model searched for. Crucial for
				// debugging "model gave up after empty result" failures —
				// without this trace we can't tell if the model used bad
				// patterns or the right patterns against the wrong tree.
				logToolCall(iter, use, result)
				resultBlocks = append(resultBlocks, result)
			}
			messages = append(messages, llm.ToolMessage{
				Role:    "user",
				Content: resultBlocks,
			})
			// Loop back for another turn.

		case "max_tokens":
			return nil, errors.New("implementer: provider hit max_tokens before completing the diff — consider bumping tier max_tokens")

		default:
			return nil, fmt.Errorf("implementer: unexpected stop_reason %q", resp.StopReason)
		}
	}

	return nil, fmt.Errorf("implementer: exhausted %d tool-use iterations without producing a diff", maxToolIterations)
}

// buildToolUseUserMessage assembles the initial user message for the
// tool-use loop. It contains everything the legacy picker+diff prompt
// contained EXCEPT the file inventory and embedded file contents —
// the model reads those via tools now instead of having them
// preloaded. This keeps the initial prompt small (3-8 KB typical)
// and lets the model fetch exactly what it needs.
func buildToolUseUserMessage(c *Context, chosen *Sketch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Issue %s/%s#%d: %s\n\n", c.Issue.Owner, c.Issue.Repo, c.Issue.Number, c.Issue.Title)
	b.WriteString(c.Issue.Body)

	if c.ScoutReport != "" {
		b.WriteString("\n\n## Scout brief\n\n")
		b.WriteString(c.ScoutReport)
	}
	if c.CriticReport != "" {
		b.WriteString("\n\n## Critic report\n\n")
		b.WriteString(c.CriticReport)
	}
	fmt.Fprintf(&b, "\n\n## Chosen sketch %d: %s\n\n", chosen.Number, chosen.Title)
	b.WriteString(chosen.Markdown)

	if len(c.Principles) > 0 {
		b.WriteString("\n\n## Engineering principles\n\n")
		for _, p := range c.Principles {
			fmt.Fprintf(&b, "- **%s** — %s\n", p.Name, p.Summary)
		}
	}

	// Thread the Clarifier session through — same as legacy path,
	// same authoritative framing.
	if strings.TrimSpace(c.ClarifierNotes) != "" {
		b.WriteString("\n\n## Clarifier session (AUTHORITATIVE human answers)\n\n")
		b.WriteString(c.ClarifierNotes)
	}

	// And Coordinator feedback from a previous attempt, if any.
	if strings.TrimSpace(c.CoordinatorFeedback) != "" {
		b.WriteString("\n\n## Coordinator feedback on your previous attempt (AUTHORITATIVE — address each item)\n\n")
		b.WriteString(c.CoordinatorFeedback)
		b.WriteString("\n\nRe-produce a COMPLETE diff that addresses every concern above.")
	}

	b.WriteString("\n\n## Your task\n\n")
	b.WriteString("You have read_file, glob, grep, and list_dir tools available. Use them freely to explore the repository and find the exact files and line numbers you need to edit. When you are ready, emit a unified git diff as your final text response — the diff is what the user will `git apply`.\n\n")
	b.WriteString("Plan your tool use: glob to find relevant files, read_file to see their contents, grep to find all call sites, then produce the diff in one pass. You do NOT need to ask permission before calling tools, and you do NOT need to narrate what you're about to do — just call the tools and then produce the diff.\n")

	return b.String()
}

// toolUseSystemPrompt is the system prompt for the tool-use path.
// Different from the legacy generateDiff prompt: no picker grammar,
// no NEED_FILES protocol, no format-prefix validator. The
// teammate-not-checklist-writer framing and the FORBIDDEN OUTPUTS
// rules stay — they're still relevant. Rule 6 becomes "your final
// assistant message must contain a unified git diff" (no prefix
// grammar required because stop_reason signals the end, not a
// string marker).
func toolUseSystemPrompt() string {
	return `You are the Implementer for aidev, a multi-agent coding tool.

You are a senior engineer pairing with a teammate. The developer has
chosen one of the Architect's sketches and is asking you to do the
work — not to write a checklist, not to hand back a draft for the
human to finish, not to file a TODO. You have direct tool access to
the repository (read_file, glob, grep, list_dir) and should use those
tools to verify every decision you make before producing the diff.

WORKFLOW:

1. Use the tools to discover the code. Typical pattern for a
   refactor or consolidation task:
     - list_dir the directory the issue body references FIRST, to
       confirm the real path and see what's actually there. If the
       issue says "files live under apps/marketing/src/lib/foo/",
       list_dir apps/marketing/src/lib/foo/ before you glob or grep
       — a directory that is not what you expect is a signal that
       the issue's path example may be partial or relative.
     - glob for candidate files ("apps/marketing/**/*.tsx").
     - grep for all call sites of the thing you're changing.
     - read_file each file you plan to modify to see exact line
       numbers and surrounding context.
     - read_file the locale / config files whose values you need
       to preserve verbatim.
   You can call multiple tools per turn — batch them when you
   already know what you need.

1a. EMPTY RESULTS ARE NOT A REASON TO GIVE UP. If a glob returns
    zero files or a grep returns zero matches, that means your
    PATTERN was wrong, not that the task is impossible. Before
    declaring ERROR on an empty result you MUST:
      - list_dir the parent directory to see what's actually there.
      - Try at least one broader pattern (strip a path segment,
        drop a suffix, or grep for a shorter substring).
      - If the issue body or the chosen sketch quotes literal file
        paths or key names, read_file those paths DIRECTLY — do not
        assume glob will find them from a guessed pattern.
    Declaring ERROR after exactly one empty tool result is a
    failure mode we are explicitly guarding against. The issue body
    and sketch usually contain the exact paths and identifiers you
    need — use them verbatim before trying to infer.

2. When you have enough context, emit a unified git diff as your
   final assistant message (without any more tool calls). The loop
   ends when your turn contains only text.

3. CRITICAL — how to actually call tools: use the native tool_call
   mechanism provided by your runtime. Do NOT emit a JSON object
   in your message text like {"name": "read_file", "arguments":
   {"path": "..."}} and expect it to be interpreted as a tool
   call. The harness expects tool calls via the structured
   tool_calls field of your response, not JSON in the content.
   If you emit raw JSON in content the harness will treat it as
   your final diff output and reject it for not being a diff.

4. CRITICAL — when you are DONE and ready to produce the diff,
   your response content must be the LITERAL unified diff text,
   starting with the line "diff --git a/...". Do NOT wrap the
   diff in a JSON object, do NOT put it in a Markdown code
   fence, do NOT prefix it with prose. The raw diff is what
   goes straight to the 'git apply' command.

OUTPUT REQUIREMENTS for the final diff:

1. Standard git unified diff format:

     diff --git a/path/to/file b/path/to/file
     --- a/path/to/file
     +++ b/path/to/file
     @@ -lineno,count +lineno,count @@
     -old line
     +new line

   For new files use /dev/null as the 'a' side.
   For deletions use /dev/null as the 'b' side.

2. Repo-relative bare paths. No leading ./, no absolute paths.

3. Minimal in scope, COMPLETE in execution. Only touch files the
   sketch actually needs — but within that scope, do the whole job.
   A diff that covers 8 of 9 locales is a regression. Half-migrated
   call sites are a regression. If the sketch asks for 9 things,
   produce 9 things.

4. Include tests when the diff adds new behavior. Tests belong in
   the same diff, not a follow-up.

5. Include docstrings / comments where the sketch implies new
   public API, matching the repo's existing conventions.

FORBIDDEN OUTPUTS — the following are failure modes:

   * Auxiliary checklist/notes files alongside the real change
     (AIDEV_TODO_*.md, NOTES.md, CHECKLIST.md, HUMAN_FOLLOWUP.md).
     The diff IS the work. If a human has to finish the job from
     your output, you have failed. Use tools to fetch the real
     values, do not punt.

   * Comments in file formats that don't support them. JSON does
     NOT have comments — // TODO inside a .json hunk is invalid
     syntax and the diff won't apply. Check the file extension
     before adding any kind of comment.

   * Fabricated or placeholder string values. If you need a
     specific translation, brand name, API identifier, or content
     literal, read_file the source that has the canonical value.
     Never invent "Kontakt Vertrieb" as a stand-in for a German
     translation that exists elsewhere in the repo.

   * Partial starting points that expect the human to finish the
     job. You are not generating homework. Finish the scope the
     sketch asked for, end to end.

   * Claiming files don't exist based on what you "know". The
     repository is a black box to you except through the tools.
     If the Clarifier cites apps/marketing/src/components/foo.tsx,
     read_file it — the file is there. If read_file returns
     "file not found", THEN you know it's missing.

Stay realistic: the user will apply this diff with git apply and
review it. A diff that applies cleanly and finishes the scope is
worth ten drafts that need human cleanup.`
}

// errDeclaredError is the sentinel wrapping an `ERROR:` escape
// hatch from the model. runWithTools uses errors.Is to
// distinguish an ERROR: (terminal, propagate immediately) from
// a generic validation miss (format-correction candidate).
var errDeclaredError = errors.New("implementer: declared ERROR")

// validateDiffResponse checks that a model's final text looks like a
// unified git diff and returns a Patch. Uses the same grammar the
// legacy path used, minus the NEED_FILES branch (not valid in
// tool-use mode). Returns an error wrapping errDeclaredError for
// ERROR: responses so the caller can distinguish them from
// format misses.
func validateDiffResponse(raw string) (*Patch, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("implementer: empty final response (no diff produced)")
	}
	raw = stripCodeFence(raw)

	if strings.HasPrefix(raw, "ERROR:") {
		reason := strings.TrimSpace(strings.TrimPrefix(firstLine(raw), "ERROR:"))
		if reason == "" {
			reason = "(no reason given)"
		}
		// Wrap errDeclaredError so runWithTools can detect this
		// via errors.Is and propagate immediately instead of
		// trying to format-correct the ERROR: back to a diff.
		return nil, fmt.Errorf("%w: %s", errDeclaredError, reason)
	}

	// Tolerate a SHORT prose preamble before `diff --git` in the
	// tool-use path — models sometimes narrate the final turn
	// ("Here's the diff:") before the real content. But only
	// match `diff --git` at the START of a line after an optional
	// prose preamble, NOT anywhere in the raw string. Using a
	// plain strings.Index would match inside a JSON string
	// literal like {"diff": "diff --git ..."} and we'd extract
	// a garbled half-JSON as the patch body.
	//
	// Valid shapes we accept:
	//
	//   diff --git a/... b/...      ← the common case
	//
	//   Here's the diff:
	//
	//   diff --git a/... b/...      ← short prose preamble
	//
	// Anything else (JSON wrapper, tool-call JSON, random
	// garbage) triggers the format-correction retry in
	// runWithTools.
	if strings.HasPrefix(raw, "diff --git") {
		return &Patch{
			Diff:         raw,
			FilesTouched: parseFilesFromDiff(raw),
		}, nil
	}
	// Prose preamble: allow up to 500 chars of prose followed by
	// a blank line followed by `diff --git` at the start of a
	// line. Anything else falls through to the error.
	if m := proseDiffRe.FindStringSubmatchIndex(raw); m != nil {
		// m[0..1] is full match; m[2..3] is the `diff --git` group.
		diffStart := m[2]
		if diffStart <= 500 {
			diff := raw[diffStart:]
			return &Patch{
				Diff:         diff,
				FilesTouched: parseFilesFromDiff(diff),
			}, nil
		}
	}
	return nil, fmt.Errorf("implementer: final response does not start with 'diff --git'; got first line: %q", firstLine(raw))
}

// proseDiffRe matches a prose preamble (any content) followed
// by a blank line and `diff --git` at the start of a line. The
// capture group is on the `diff --git` start so we can extract
// the diff body without the prose prefix. We use `(?m)` so `^`
// matches at the start of a line, not just the start of the
// whole string.
var proseDiffRe = regexp.MustCompile(`(?m)(^diff --git )`)

// buildFormatCorrectionMessage composes the corrective user
// message we send to the model when its end_turn content isn't
// a valid diff. Quotes the broken output verbatim (truncated to
// keep prompt size bounded) and tells the model exactly what
// the next response must look like.
//
// Pattern is the same as the legacy tryGenerateDiff format
// correction (#34), adapted for the tool-use loop: we can
// mention tools because the model has real tool access here,
// so the correction allows either "try more tool_use turns" or
// "emit a clean diff" or "emit ERROR: <reason>".
func buildFormatCorrectionMessage(brokenContent string, validationErr error) string {
	var b strings.Builder
	b.WriteString("## Your previous response was not a valid final output\n\n")
	fmt.Fprintf(&b, "The validator rejected your end_turn response with: %v\n\n", validationErr)
	b.WriteString("What you wrote (first 500 chars):\n\n> ")
	b.WriteString(strings.ReplaceAll(truncateForError(brokenContent, 500), "\n", "\n> "))
	b.WriteString("\n\n")
	b.WriteString("Your next response MUST be EXACTLY ONE of:\n\n")
	b.WriteString("1. A complete unified git diff starting with `diff --git a/...`. ")
	b.WriteString("This is the success path — the diff is what the user will `git apply`.\n\n")
	b.WriteString("2. More tool calls (read_file, glob, grep, list_dir) if you need more ")
	b.WriteString("repository context before you can produce the diff. Use the native ")
	b.WriteString("tool-call mechanism, not a JSON object in your content.\n\n")
	b.WriteString("3. A single line `ERROR: <reason>` if the task is genuinely impossible ")
	b.WriteString("(e.g. sketch contradicts itself, required files don't exist).\n\n")
	b.WriteString("Do NOT emit prose describing what you would do. Do NOT emit a JSON ")
	b.WriteString("wrapper around the diff (like `{\"diff\": \"...\"}`). The diff itself ")
	b.WriteString("must be the literal content of your response.\n")
	return b.String()
}

// truncateForError clips a string to n bytes, appending an
// ellipsis marker if truncation happened. Used for error
// messages and correction prompts so a runaway output can't
// balloon the log or the next turn's context.
func truncateForError(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated at " + fmt.Sprint(n) + " bytes]"
}

// logToolCall writes a single-line breadcrumb to stderr for one
// tool invocation. Format: "aidev: implementer: tool iter=N
// name=<tool> input=<json-one-line> result=<short summary>".
// The result summary distinguishes between empty results (the
// common "gave up after one search" failure mode), error results
// from the executor, and non-empty results — truncated so the
// breadcrumb stays readable in a terminal scrollback buffer.
func logToolCall(iter int, use llm.ToolUse, result llm.ToolContentBlock) {
	// Collapse the json input onto one line so the breadcrumb is
	// grep-friendly. json.RawMessage is already compact in most
	// cases; strip newlines defensively.
	input := strings.ReplaceAll(string(use.Input), "\n", " ")
	input = strings.ReplaceAll(input, "\t", " ")
	if len(input) > 240 {
		input = input[:240] + "..."
	}

	resultSummary := summariseToolResult(result)
	fmt.Fprintf(os.Stderr, "aidev: implementer: tool iter=%d name=%s input=%s result=%s\n",
		iter, use.Name, input, resultSummary)
}

// summariseToolResult produces a short, single-line description of
// a tool's output for the breadcrumb log. It deliberately flags
// empty/error results loudly because those are the signals that
// tell us why the model gave up — a non-verbose "ok: 12 bytes" is
// uninteresting, but "empty (no matches)" on a grep is diagnostic.
func summariseToolResult(r llm.ToolContentBlock) string {
	body := strings.TrimSpace(r.ToolResultContent)
	switch {
	case r.ToolResultIsError:
		first := body
		if len(first) > 120 {
			first = first[:120] + "..."
		}
		return "ERROR: " + first
	case body == "":
		return "empty"
	case len(body) < 120:
		return "ok: " + strings.ReplaceAll(body, "\n", " ⏎ ")
	default:
		return fmt.Sprintf("ok: %d bytes, %d lines", len(body), strings.Count(body, "\n")+1)
	}
}
