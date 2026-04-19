// Package completion provides shell completion installation for aidev
package completion

import (
	"fmt"
	"os"
	"path/filepath"
)

// Installer handles shell completion installation
type Installer struct {
	homeDir string
}

// NewInstaller creates a new completion installer
func NewInstaller() (*Installer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home directory: %w", err)
	}
	return &Installer{homeDir: home}, nil
}

// InstallBashCompletion installs bash completion
func (ci *Installer) InstallBashCompletion() error {
	// Try system-wide first, then user-local
	systemPath := "/etc/bash_completion.d/aidev"
	userPath := filepath.Join(ci.homeDir, ".bash_completion.d", "aidev")
	fallbackPath := filepath.Join(ci.homeDir, ".bashrc")

	// Check if system directory is writable
	if _, err := os.Stat("/etc/bash_completion.d"); err == nil {
		if err := ci.installCompletionFile("bash", systemPath); err == nil {
			fmt.Printf("Installed bash completion to %s\n", systemPath)
			return nil
		}
	}

	// Try user-local directory
	userDir := filepath.Dir(userPath)
	if err := os.MkdirAll(userDir, 0755); err == nil {
		if err := ci.installCompletionFile("bash", userPath); err == nil {
			fmt.Printf("Installed bash completion to %s\n", userPath)
			return ci.addBashSource(userPath, fallbackPath)
		}
	}

	// Fallback to adding to .bashrc
	return ci.addBashCompletionToBashrc(fallbackPath)
}

// InstallZshCompletion installs zsh completion
func (ci *Installer) InstallZshCompletion() error {
	// Try user-local completion directory first
	zshCompDir := filepath.Join(ci.homeDir, ".zsh", "completions")
	zshPath := filepath.Join(zshCompDir, "_aidev")

	if err := os.MkdirAll(zshCompDir, 0755); err == nil {
		if err := ci.installCompletionFile("zsh", zshPath); err == nil {
			fmt.Printf("Installed zsh completion to %s\n", zshPath)
			return ci.addZshCompletionToConfig()
		}
	}

	// Try system-wide
	systemPath := "/usr/share/zsh/site-functions/_aidev"
	if _, err := os.Stat("/usr/share/zsh/site-functions"); err == nil {
		if err := ci.installCompletionFile("zsh", systemPath); err == nil {
			fmt.Printf("Installed zsh completion to %s\n", systemPath)
			return ci.addZshCompletionToConfig()
		}
	}

	// Fallback to adding to .zshrc
	return ci.addZshCompletionToZshrc()
}

// installCompletionFile copies the completion file to the target path
func (ci *Installer) installCompletionFile(shell, targetPath string) error {
	// Read the embedded completion file
	var content string
	switch shell {
	case "bash":
		content = bashCompletion
	case "zsh":
		content = zshCompletion
	default:
		return fmt.Errorf("unsupported shell: %s", shell)
	}

	// Write to target file
	if err := os.WriteFile(targetPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write completion file: %w", err)
	}

	return nil
}

// addBashSource adds source line to .bashrc
func (ci *Installer) addBashSource(completionPath, bashrcPath string) error {
	content, err := os.ReadFile(bashrcPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .bashrc: %w", err)
	}

	bashrcContent := string(content)
	sourceLine := fmt.Sprintf("source %s", completionPath)

	// Check if already sourced
	if contains(bashrcContent, sourceLine) {
		return nil
	}

	// Add source line
	newContent := bashrcContent
	if newContent != "" && !endsWith(newContent, "\n") {
		newContent += "\n"
	}
	newContent += fmt.Sprintf("# aidev bash completion\n%s\n", sourceLine)

	if err := os.WriteFile(bashrcPath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("write .bashrc: %w", err)
	}

	fmt.Printf("Added bash completion source to %s\n", bashrcPath)
	return nil
}

// addBashCompletionToBashrc adds inline completion to .bashrc
func (ci *Installer) addBashCompletionToBashrc(bashrcPath string) error {
	content, err := os.ReadFile(bashrcPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .bashrc: %w", err)
	}

	bashrcContent := string(content)
	marker := "# aidev bash completion"

	// Check if already added
	if contains(bashrcContent, marker) {
		return nil
	}

	// Add completion inline
	newContent := bashrcContent
	if newContent != "" && !endsWith(newContent, "\n") {
		newContent += "\n"
	}
	newContent += fmt.Sprintf(`%s
if command -v aidev &> /dev/null; then
    eval "$(aidev completion bash)"
fi
`, marker)

	if err := os.WriteFile(bashrcPath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("write .bashrc: %w", err)
	}

	fmt.Printf("Added bash completion to %s\n", bashrcPath)
	return nil
}

// addZshCompletionToConfig adds completion to zsh config
func (ci *Installer) addZshCompletionToConfig() error {
	zshrcPath := filepath.Join(ci.homeDir, ".zshrc")
	return ci.addZshCompletionToFile(zshrcPath)
}

// addZshCompletionToZshrc adds completion to .zshrc
func (ci *Installer) addZshCompletionToZshrc() error {
	zshrcPath := filepath.Join(ci.homeDir, ".zshrc")
	return ci.addZshCompletionToFile(zshrcPath)
}

// addZshCompletionToFile adds zsh completion to a config file
func (ci *Installer) addZshCompletionToFile(zshrcPath string) error {
	content, err := os.ReadFile(zshrcPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read zsh config: %w", err)
	}

	zshrcContent := string(content)
	marker := "# aidev zsh completion"

	// Check if already added
	if contains(zshrcContent, marker) {
		return nil
	}

	// Add completion
	newContent := zshrcContent
	if newContent != "" && !endsWith(newContent, "\n") {
		newContent += "\n"
	}
	newContent += fmt.Sprintf(`%s
if command -v aidev &> /dev/null; then
    eval "$(aidev completion zsh)"
fi
`, marker)

	if err := os.WriteFile(zshrcPath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("write zsh config: %w", err)
	}

	fmt.Printf("Added zsh completion to %s\n", zshrcPath)
	return nil
}

// InstallAll installs completions for detected shells
func (ci *Installer) InstallAll() error {
	shell := os.Getenv("SHELL")
	
	fmt.Printf("Installing shell completions...\n")

	switch {
	case endsWith(shell, "bash"):
		return ci.InstallBashCompletion()
	case endsWith(shell, "zsh"):
		return ci.InstallZshCompletion()
	default:
		// Install both if shell detection fails
		fmt.Printf("Could not detect shell, installing both bash and zsh completions...\n")
		if err := ci.InstallBashCompletion(); err != nil {
			fmt.Printf("Warning: bash completion failed: %v\n", err)
		}
		if err := ci.InstallZshCompletion(); err != nil {
			fmt.Printf("Warning: zsh completion failed: %v\n", err)
		}
		return nil
	}
}

// Helper functions

func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr) >= 0
}

func findSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// Embedded completion scripts
const bashCompletion = `# aidev bash completion script
# Install by placing this file in /etc/bash_completion.d/aidev or ~/.bash_completion.d/aidev
# Or add to your ~/.bashrc: source /path/to/aidev/completion/bash

_aidev_completion() {
    local cur prev words cword
    _init_completion || return

    case "${prev}" in
        aidev)
            COMPREPLY=($(compgen -W "--version --help -issue -repo -config -headless -auto -n -sketch -interactive -force-verdict -skip-doctor -no-audit-trail doctor charter test review plugin install init followups clarify config ollama completion" -- "${cur}"))
            ;;
        -issue)
            if [[ "${cur}" == https://* ]]; then
                COMPREPLY=($(compgen -W "github.com" -- "${cur}"))
            else
                COMPREPLY=($(compgen -W "https://github.com" -- "${cur}"))
            fi
            ;;
        -repo|-config|--dir|-dir)
            COMPREPLY=($(compgen -d -- "${cur}"))
            ;;
        -n|-sketch|-round)
            COMPREPLY=($(compgen -W "1 2 3 4 5 6 7 8 9" -- "${cur}"))
            ;;
        -force-verdict)
            COMPREPLY=($(compgen -W "build defer kill" -- "${cur}"))
            ;;
        --profile)
            # The five shipped profiles — keep in sync with
            # cmd/aidev/init_subcommand.go shippedInitProfiles.
            COMPREPLY=($(compgen -W "cloud-only default high-vram low-vram offline" -- "${cur}"))
            ;;
        doctor)
            COMPREPLY=($(compgen -W "-config --no-spawn --help" -- "${cur}"))
            ;;
        charter|test|clarify)
            COMPREPLY=($(compgen -W "-repo -config --help" -- "${cur}"))
            ;;
        review)
            COMPREPLY=($(compgen -W "-issue -repo -config -patch -triage -round -ci-status -prev-actions --help" -- "${cur}"))
            ;;
        install)
            COMPREPLY=($(compgen -W "--force --dir --help" -- "${cur}"))
            ;;
        init)
            COMPREPLY=($(compgen -W "--profile --yes --force --dir --skip-doctor --help" -- "${cur}"))
            ;;
        plugin)
            COMPREPLY=($(compgen -W "install uninstall --help" -- "${cur}"))
            ;;
        followups)
            COMPREPLY=($(compgen -W "-repo -target --file-issues --help" -- "${cur}"))
            ;;
        config)
            COMPREPLY=($(compgen -W "show profile set edit doctor --help" -- "${cur}"))
            ;;
        ollama)
            COMPREPLY=($(compgen -W "list recommend validate health profile --help" -- "${cur}"))
            ;;
        completion)
            COMPREPLY=($(compgen -W "install bash zsh --script --help" -- "${cur}"))
            ;;
        profile)
            if [[ "${words[*]}" == *"config profile"* ]]; then
                COMPREPLY=($(compgen -W "list use --help" -- "${cur}"))
            fi
            ;;
        set)
            if [[ "${words[*]}" == *"config set"* ]]; then
                COMPREPLY=($(compgen -W "scout critic architect charter implementer reviewer tester coordinator" -- "${cur}"))
            fi
            ;;
        recommend)
            if [[ "${words[*]}" == *"ollama recommend"* ]]; then
                COMPREPLY=($(compgen -W "small medium large" -- "${cur}"))
            fi
            ;;
        validate)
            if [[ "${words[*]}" == *"ollama validate"* ]]; then
                COMPREPLY=($(compgen -W "--require-tools --help" -- "${cur}"))
            fi
            ;;
        *)
            if [[ "${cur}" == -* ]]; then
                COMPREPLY=($(compgen -W "--version --help -issue -repo -config -headless -auto -n -sketch -interactive -force-verdict -skip-doctor -no-audit-trail" -- "${cur}"))
            else
                COMPREPLY=($(compgen -W "doctor charter test review plugin install init followups clarify config ollama completion" -- "${cur}"))
            fi
            ;;
    esac
} &&
complete -F _aidev_completion aidev`

const zshCompletion = `#compdef aidev
# aidev zsh completion script

_aidev() {
    local -a commands
    commands=(
        'doctor:Run environment health checks'
        'charter:Generate repository charter'
        'test:Run test suite'
        'review:Review generated patches'
        'plugin:Manage Claude Code plugins'
        'install:Install default configuration'
        'init:Guided first-run onboarding (profile pick + doctor gate)'
        'followups:Manage follow-up issues'
        'clarify:Clarify ambiguous requirements'
        'config:Manage configuration'
        'ollama:Manage Ollama models'
        'completion:Install shell tab completion'
    )

    local -a main_options
    main_options=(
        '--version[Show version information]'
        '--help[Show help information]'
        '-issue[GitHub issue URL]:issue:_urls'
        '-repo[Repository path]:directory:_directories'
        '-config[Config directory]:directory:_directories'
        '-headless[Run without TUI]'
        '-auto[Auto-run architect when critic recommends build]'
        '-interactive[Pause at Architect for manual sketch pick]'
        '-n[Number of architect sketches]:number:(1 2 3 4 5 6 7 8 9)'
        '-sketch[Auto-select sketch number]:number:(1 2 3 4 5 6 7 8 9)'
        '-force-verdict[Force critic verdict]:verdict:(build defer kill)'
        '-skip-doctor[Skip the startup precondition audit]'
        '-no-audit-trail[Disable posting progress to the GitHub issue]'
    )

    local context state line
    typeset -A opt_args

    _arguments -C \
        $main_options \
        '1: :->command' \
        '*: :->args' && ret=0

    case $state in
        command)
            _describe 'command' commands
            ;;
        args)
            case $line[1] in
                charter|test|clarify)
                    _arguments \
                        '-repo[Repository path]:directory:_directories' \
                        '-config[Config directory]:directory:_directories' \
                        '--help[Show help]'
                    ;;
                doctor)
                    _arguments \
                        '-config[Config directory]:directory:_directories' \
                        '--no-spawn[Disable auto-spawn of ollama serve]' \
                        '--help[Show help]'
                    ;;
                review)
                    _arguments \
                        '-issue[GitHub issue URL]:issue:_urls' \
                        '-repo[Repository path]:directory:_directories' \
                        '-config[Config directory]:directory:_directories' \
                        '-patch[Path to patch file]:patch:_files' \
                        '-triage[Emit JSON for review->fix loop]' \
                        '-round[Review-loop round number]:round:(1 2 3 4 5)' \
                        '-ci-status[Path to CI status JSON]:ci-status:_files' \
                        '-prev-actions[Path to prior-round TriageActions JSON]:prev-actions:_files' \
                        '--help[Show help]'
                    ;;
                install)
                    _arguments \
                        '--force[Overwrite existing config files]' \
                        '--dir[Target directory]:directory:_directories' \
                        '--help[Show help]'
                    ;;
                init)
                    _arguments \
                        '--profile[Activate profile without prompting]:profile:(cloud-only default high-vram low-vram offline)' \
                        '--yes[Non-interactive mode, assume yes]' \
                        '--force[Overwrite existing models.yaml]' \
                        '--dir[Target config directory]:directory:_directories' \
                        '--skip-doctor[Skip the final doctor verification]' \
                        '--help[Show help]'
                    ;;
                plugin)
                    _arguments \
                        '1:plugin_action:(install uninstall)' \
                        '--force[Force overwrite existing files]' \
                        '--dir[Target directory]:directory:_directories' \
                        '--help[Show help]'
                    ;;
                followups)
                    _arguments \
                        '-repo[Repository path]:directory:_directories' \
                        '-target[Target GitHub repo]:target:_github_repos' \
                        '--file-issues[Actually file issues]' \
                        '--help[Show help]'
                    ;;
                config)
                    local -a config_commands
                    config_commands=(
                        'show:Show active profile'
                        'profile:Manage profiles'
                        'set:Set role configuration'
                        'edit:Edit configuration file'
                        'doctor:Validate configuration'
                    )
                    _describe 'config command' config_commands
                    ;;
                ollama)
                    local -a ollama_commands
                    ollama_commands=(
                        'list:List available models'
                        'recommend:Recommend models'
                        'validate:Validate model'
                        'health:Check daemon health'
                        'profile:Show usage profile'
                    )
                    _describe 'ollama command' ollama_commands
                    ;;
                completion)
                    _arguments \
                        '1:completion_action:(install bash zsh)' \
                        '--script[Output the script instead of installing]' \
                        '--help[Show help]'
                    ;;
            esac
            ;;
    esac
}

_urls() {
    local -a urls
    urls=('https://github.com')
    _describe 'GitHub URL' urls
}

_github_repos() {
    local -a repos
    repos=('owner/repo:Repository in owner/repo format')
    _describe 'GitHub repository' repos
}

_aidev "$@"`
