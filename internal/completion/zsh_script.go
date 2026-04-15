package completion

// GenerateZshCompletion generates the zsh completion script output
func GenerateZshCompletion() string {
	return `#compdef aidev
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
        'followups:Manage follow-up issues'
        'clarify:Clarify ambiguous requirements'
        'config:Manage configuration'
        'ollama:Manage Ollama models'
        'completion:Install shell completion'
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
        '-n[Number of architect sketches]:number:(1 2 3 4 5 6 7 8 9)'
        '-sketch[Auto-select sketch number]:number:(1 2 3 4 5 6 7 8 9)'
        '-force-verdict[Force critic verdict]:verdict:(build defer kill)'
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
                doctor|charter|test|review|install|clarify)
                    _arguments \
                        '-repo[Repository path]:directory:_directories' \
                        '-config[Config directory]:directory:_directories' \
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
                    local -a completion_commands
                    completion_commands=(
                        'bash:Install bash completion'
                        'zsh:Install zsh completion'
                        'install:Install completions for detected shell'
                    )
                    _describe 'completion command' completion_commands
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

compdef _aidev aidev
`
}

// GenerateBashCompletion generates the bash completion script output
func GenerateBashCompletion() string {
	return `# aidev bash completion script
_aidev_completion() {
    local cur prev words cword
    _init_completion || return

    case "${prev}" in
        aidev)
            COMPREPLY=($(compgen -W "--version --help -issue -repo -config -headless -auto -n -sketch -force-verdict doctor charter test review plugin install followups clarify config ollama completion" -- "${cur}"))
            ;;
        -issue)
            if [[ "${cur}" == https://* ]]; then
                COMPREPLY=($(compgen -W "github.com" -- "${cur}"))
            else
                COMPREPLY=($(compgen -W "https://github.com" -- "${cur}"))
            fi
            ;;
        -repo|-config)
            COMPREPLY=($(compgen -d -- "${cur}"))
            ;;
        -n|-sketch)
            COMPREPLY=($(compgen -W "1 2 3 4 5 6 7 8 9" -- "${cur}"))
            ;;
        -force-verdict)
            COMPREPLY=($(compgen -W "build defer kill" -- "${cur}"))
            ;;
        doctor|charter|test|review|install|clarify)
            COMPREPLY=($(compgen -W "-repo -config --help" -- "${cur}"))
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
            COMPREPLY=($(compgen -W "bash zsh install --help" -- "${cur}"))
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
                COMPREPLY=($(compgen -W "--version --help -issue -repo -config -headless -auto -n -sketch -force-verdict" -- "${cur}"))
            else
                COMPREPLY=($(compgen -W "doctor charter test review plugin install followups clarify config ollama completion" -- "${cur}"))
            fi
            ;;
    esac
} &&
complete -F _aidev_completion aidev`
}
