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
// protocol, NO harvest step, NO missing-path tracking, and NO
// format-correction retry. All of those were workarounds for the
// missing tool-use capability. The model now has direct access to
// the filesystem via tools and the conversation state is the only
// thing the loop has to manage.
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
			// Model is done iterating. Its final text is the patch.
			return validateDiffResponse(resp.Content)

		case "tool_use":
			if len(resp.ToolUses) == 0 {
				return nil, errors.New("implementer: stop_reason=tool_use but no tool_use blocks in response")
			}
			// Execute every pending tool call and build the
			// follow-up user message with tool_result blocks.
			resultBlocks := make([]llm.ToolContentBlock, 0, len(resp.ToolUses))
			for _, use := range resp.ToolUses {
				resultBlocks = append(resultBlocks, exec.Execute(use))
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
     - glob for candidate files ("apps/marketing/**/*.tsx")
     - grep for all call sites of the thing you're changing
     - read_file each file you plan to modify to see exact line
       numbers and surrounding context
     - read_file the locale / config files whose values you need
       to preserve verbatim
   You can call multiple tools per turn — batch them when you
   already know what you need.

2. When you have enough context, emit a unified git diff as your
   final assistant message (without any more tool calls). The loop
   ends when your turn contains only text.

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

// validateDiffResponse checks that a model's final text looks like a
// unified git diff and returns a Patch. Uses the same grammar the
// legacy path used, minus the NEED_FILES branch (not valid in
// tool-use mode) and minus the format-correction retry (the model
// can iterate inside the tool loop, so a bad final turn is a real
// failure, not a format slip).
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
		return nil, fmt.Errorf("implementer: declared ERROR: %s", reason)
	}

	// Tolerate a short prose preamble before `diff --git` in the
	// tool-use path — models sometimes narrate the final turn
	// ("Here's the diff:") before the real content. We look for
	// the first `diff --git` anywhere in the response and treat
	// everything from there to the end as the patch body.
	idx := strings.Index(raw, "diff --git")
	if idx < 0 {
		return nil, fmt.Errorf("implementer: final response contains no 'diff --git' marker; got first line: %q", firstLine(raw))
	}
	diff := raw[idx:]

	return &Patch{
		Diff:         diff,
		FilesTouched: parseFilesFromDiff(diff),
	}, nil
}
