package completion

// GenerateBashCompletion and GenerateZshCompletion are the public
// accessors for `aidev completion <shell> --script`. Both point at
// the single source of truth in completion.go (bashCompletion and
// zshCompletion constants) so the installed script and the
// --script output never drift apart. If you update either
// constant, both paths pick up the change automatically.

// GenerateBashCompletion returns the bash completion script text.
func GenerateBashCompletion() string {
	return bashCompletion
}

// GenerateZshCompletion returns the zsh completion script text.
func GenerateZshCompletion() string {
	return zshCompletion
}
