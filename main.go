package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/driftywinds/manyup/internal/config"
	"github.com/driftywinds/manyup/internal/plugin"
	"github.com/driftywinds/manyup/internal/services"
	"github.com/driftywinds/manyup/internal/update"
	"github.com/driftywinds/manyup/internal/uploader"
)

// credentialTimeout bounds the pre-save credential check.
const credentialTimeout = 20 * time.Second

var version = "0.1.0"

func main() {
	// A Windows update leaves the previous binary beside this one; drop it now
	// that the process holding it has exited.
	update.CleanupPreviousBinary()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	// Handle -U / --update flags
	if os.Args[1] == "-U" || os.Args[1] == "--update" {
		cmdUpdate()
		return
	}

	switch os.Args[1] {
	case "upload":
		cmdUpload()
	case "services":
		cmdServices()
	case "config":
		cmdConfig()
	case "interim":
		cmdInterim()
	case "completion":
		cmdCompletion()
	case "__complete":
		cmdComplete()
	case "where":
		cmdWhere()
	case "version":
		fmt.Printf("manyup %s\n", version)
	case "update", "-U", "--update":
		cmdUpdate()
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`manyup — Blazing fast multi-service file uploader

USAGE:
  manyup <command> [options]

COMMANDS:
  upload <file> [file...]     Upload file(s) to selected services
  upload <file> -s <service>...  Upload only to these services for this run
  services                    List available service plugins
  config menu                 Interactive service selector
  config set <service>                 Prompt for the API token (quote-free, any characters)
  config set <service> <value>         Set the API token (key defaults to API_TOKEN)
  config set <service> <key> <value>   Set any credential or option explicitly
  config show                 Show current configuration
  config mode <parallel|sequential>    Set upload mode
  config select <service>     Toggle a service on/off
  config retry <max> [seconds]  Retries per service + base backoff delay
  interim list                List the default and configured interim instances
  interim add <url> [name]    Add a self-hosted interim instance (optional hash)
  interim remove <name>       Remove a configured interim instance
  completion <shell>          Print a completion script (bash|zsh|fish|powershell|clink)
  where                       Show where manyup is installed and where its config lives
  version                     Print version
  update, -U, --update       Check for and apply updates

Credentials are checked against the service before they are saved, so a mistyped
token is never persisted.

EXAMPLES:
  # Interactive service selector
  manyup config menu

  # Configure a service — the key defaults to API_TOKEN
  manyup config set mocha mocha_xxxxxxxxxxxxxxxxx
  manyup config set gofile API_TOKEN mytoken123

  # Add a self-hosted interim instance (prompts for an optional hash)
  manyup interim add https://files.example.com friends
  manyup config select interim@friends

  # Enable tab completion (then restart your shell)
  manyup completion powershell | Out-String | Invoke-Expression
  echo 'eval "$(manyup completion bash)"' >> ~/.bashrc

  # Upload a file
  manyup upload myfile.zip

  # Upload multiple files
  manyup upload *.zip

  # Upload only to specific services, without changing the saved selection
  manyup upload movie.mkv -s abyss anonmp4
  manyup upload movie.mkv --sources=abyss,anonmp4

ENVIRONMENT VARIABLES:
  Credentials can also be set via env vars:
    MANYUP_<SERVICE>_API_TOKEN  e.g. MANYUP_GOFILE_API_TOKEN=xxx
`)
}

type uploadOptions struct {
	files      []string
	sources    []string
	hasSources bool
}

// parseUploadArgs parses file paths plus an optional -s/--sources override.
//
// The override accepts both forms:
//
//	manyup upload file.mkv -s abyss anonmp4
//	manyup upload file.mkv --sources=abyss,anonmp4
//
// Source names are validated against the registry. In space-separated mode a
// token that is not a service name ends the source list; it is accepted as a
// file only when it exists, so a misspelled source is reported as such instead
// of becoming a nonexistent upload target.
func parseUploadArgs(args []string, registry *plugin.Registry) (uploadOptions, error) {
	var opts uploadOptions

	known := make(map[string]string, len(registry.Names()))
	for _, name := range registry.Names() {
		known[strings.ToLower(name)] = name
	}

	addSource := func(raw string) error {
		name := strings.TrimSpace(raw)
		if name == "" {
			return fmt.Errorf("empty service name in --sources")
		}
		canonical, ok := known[strings.ToLower(name)]
		if !ok {
			return fmt.Errorf("unknown source %q (available: %s)", name, strings.Join(registry.Names(), ", "))
		}
		for _, existing := range opts.sources {
			if existing == canonical {
				return nil
			}
		}
		opts.sources = append(opts.sources, canonical)
		return nil
	}

	addSourceList := func(raw string) error {
		for _, part := range strings.Split(raw, ",") {
			if err := addSource(part); err != nil {
				return err
			}
		}
		return nil
	}

	inSources := false
	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			opts.files = append(opts.files, args[i+1:]...)
			i = len(args)

		case arg == "-s" || arg == "--sources":
			opts.hasSources = true
			inSources = true

		case strings.HasPrefix(arg, "--sources="):
			opts.hasSources = true
			inSources = false
			if err := addSourceList(strings.TrimPrefix(arg, "--sources=")); err != nil {
				return opts, err
			}

		case strings.HasPrefix(arg, "-s="):
			opts.hasSources = true
			inSources = false
			if err := addSourceList(strings.TrimPrefix(arg, "-s=")); err != nil {
				return opts, err
			}

		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)

		default:
			if !inSources {
				opts.files = append(opts.files, arg)
				continue
			}

			if canonical, ok := known[strings.ToLower(arg)]; ok {
				if err := addSource(canonical); err != nil {
					return opts, err
				}
				continue
			}

			// A non-service token ends the source list. It is treated as a file
			// only if it exists; otherwise it is almost certainly a typo in a
			// source name.
			if _, err := os.Stat(arg); err == nil {
				inSources = false
				opts.files = append(opts.files, arg)
				continue
			}
			return opts, fmt.Errorf("unknown source %q (available: %s)", arg, strings.Join(registry.Names(), ", "))
		}
	}

	if len(opts.files) == 0 {
		return opts, fmt.Errorf("no files specified")
	}
	if opts.hasSources && len(opts.sources) == 0 {
		return opts, fmt.Errorf("-s/--sources requires at least one service")
	}
	return opts, nil
}

func cmdUpload() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: manyup upload <file> [file...] [-s|--sources <service>...]")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	registry := services.AllWithConfig(cfg)
	opts, err := parseUploadArgs(os.Args[2:], registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintln(os.Stderr, "Usage: manyup upload <file> [file...] [-s|--sources <service>...]")
		os.Exit(1)
	}

	if !opts.hasSources && len(cfg.SelectedServices) == 0 {
		fmt.Fprintln(os.Stderr, "No services selected. Use 'manyup config select <service>' first.")
		os.Exit(1)
	}

	mgr := uploader.New(registry, cfg)
	if opts.hasSources {
		mgr.SetSelectedServices(opts.sources)
	}

	// Set up context with signal handling for graceful abort.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nAborted.")
		cancel()
	}()

	files := opts.files
	progressCh := make(chan uploader.Progress, 100)
	display := newProgressDisplay()

	// Render progress bars in a goroutine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range progressCh {
			display.handle(p)
		}
	}()

	for _, filePath := range files {
		fmt.Printf("⬆  Uploading: %s\n", filePath)
		result, err := mgr.UploadFile(ctx, filePath, progressCh)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}
		printResult(result)
	}

	close(progressCh)
	<-done
}

func cmdServices() {
	// A broken config should not hide the built-in services; fall back to the
	// defaults and let the listing report what it can.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not read config: %v\n", err)
		cfg = nil
	}
	registry := services.AllWithConfig(cfg)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESCRIPTION\tAUTH REQUIRED")
	fmt.Fprintln(w, "----\t-----------\t-------------")
	for _, name := range registry.Names() {
		svc, _ := registry.Get(name)
		auth := "No"
		if len(svc.RequiredCredentials()) > 0 {
			auth = strings.Join(svc.RequiredCredentials(), ", ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", svc.DisplayName(), svc.Description(), auth)
	}
	w.Flush()
}

// cmdWhere reports where this executable and its config file live, and whether
// the install directory is reachable from PATH.
func cmdWhere() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not determine the manyup executable path: %v\n", err)
		os.Exit(1)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(exe); resolveErr == nil {
		exe = resolved
	}

	dir := filepath.Dir(exe)
	fmt.Printf("manyup %s\n", version)
	fmt.Printf("  binary:  %s\n", exe)
	fmt.Printf("  config:  %s\n", config.Path())

	if onPath(dir) {
		fmt.Println("  on PATH: yes")
		return
	}

	fmt.Println("  on PATH: no")
	fmt.Printf("\nTo run manyup from anywhere, add this folder to your PATH:\n  %s\n", dir)
	fmt.Println(`See the README section "Add manyup to PATH".`)
}

// onPath reports whether dir is one of the entries in PATH. Windows paths are
// compared case-insensitively.
func onPath(dir string) bool {
	want := filepath.Clean(dir)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(entry), want) {
			return true
		}
	}
	return false
}

func cmdConfig() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	args := os.Args[2:]
	if len(args) == 0 {
		cmdConfigMenu()
		return
	}

	switch args[0] {
	case "menu":
		cmdConfigMenu()
	case "set":
		service, key, value, parseErr := parseSetArgs(args[1:])
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", parseErr)
			fmt.Fprintln(os.Stderr, "Usage: manyup config set <service> <value>")
			fmt.Fprintln(os.Stderr, "       manyup config set <service> <key> <value>")
			os.Exit(1)
		}

		registry := services.AllWithConfig(cfg)
		svc, ok := registry.Get(service)
		if !ok {
			fmt.Fprintf(os.Stderr, "Unknown service: %s (available: %s)\n",
				service, strings.Join(registry.Names(), ", "))
			os.Exit(1)
		}

		// No value on the command line, or "-": read it from stdin. Tokens often
		// contain characters the shell eats (| & < > ^), and those never reach
		// us at all when they are part of an argument.
		if value == "" || value == "-" {
			fmt.Printf("Paste the %s for %s and press Enter:\n> ", key, svc.DisplayName())
			value, err = readSecretValue(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
				os.Exit(1)
			}
		}

		// An interim instance origin is validated before it is saved so a typo
		// cannot silently point uploads at the wrong host.
		if strings.EqualFold(key, "BASE_URL") && services.IsInterimService(service) {
			origin, _, originErr := services.NormalizeInterimURL(value)
			if originErr != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", originErr)
				os.Exit(1)
			}
			value = origin
		}

		// Check the credential first: a token the service refuses is a typo,
		// and persisting it would only postpone the failure to upload time.
		note, verifyErr := authenticateCredential(svc, key, value)
		if verifyErr != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", verifyErr)
			fmt.Fprintln(os.Stderr, "Credential was not saved.")
			os.Exit(1)
		}

		// Credentials and options are stored separately: plugins read tokens
		// from Credentials and settings such as BASE_URL or path from Options.
		if isCredentialKey(key) {
			cfg.SetCredential(service, key, value)
		} else {
			cfg.SetOption(service, key, value)
		}
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
			os.Exit(1)
		}
		if note != "" {
			fmt.Printf("✓ Set %s/%s %s\n", service, key, note)
		} else {
			fmt.Printf("✓ Set %s/%s\n", service, key)
		}

	case "show":
		printConfig(cfg)

	case "mode":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Current mode: %s\n", cfg.UploadMode)
			fmt.Fprintln(os.Stderr, "Usage: manyup config mode <parallel|sequential>")
			os.Exit(1)
		}
		mode := config.UploadMode(args[1])
		if mode != config.ModeParallel && mode != config.ModeSequential {
			fmt.Fprintln(os.Stderr, "Mode must be 'parallel' or 'sequential'")
			os.Exit(1)
		}
		cfg.UploadMode = mode
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ Upload mode set to %s\n", mode)

	case "retry":
		if len(args) < 2 {
			maxRetries, delay := cfg.RetrySettings()
			fmt.Printf("Current: %d retries per service, %ds base delay\n", maxRetries, delay)
			fmt.Fprintln(os.Stderr, "Usage: manyup config retry <max-retries> [base-delay-seconds]")
			os.Exit(1)
		}
		maxRetries, err := strconv.Atoi(args[1])
		if err != nil || maxRetries < 0 {
			fmt.Fprintln(os.Stderr, "Max retries must be a non-negative number (0 disables retries)")
			os.Exit(1)
		}
		_, delay := cfg.RetrySettings()
		if len(args) >= 3 {
			d, convErr := strconv.Atoi(args[2])
			if convErr != nil || d < 0 {
				fmt.Fprintln(os.Stderr, "Base delay must be a non-negative number of seconds")
				os.Exit(1)
			}
			delay = d
		}
		cfg.SetRetrySettings(maxRetries, delay)
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ Retries set: %d per service, %ds base delay (exponential backoff)\n", maxRetries, delay)

	case "select":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: manyup config select <service>")
			os.Exit(1)
		}
		service := args[1]
		registry := services.AllWithConfig(cfg)
		if _, ok := registry.Get(service); !ok {
			fmt.Fprintf(os.Stderr, "Unknown service: %s (available: %s)\n",
				service, strings.Join(registry.Names(), ", "))
			os.Exit(1)
		}
		cfg.ToggleService(service)
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
			os.Exit(1)
		}
		state := "enabled"
		found := false
		for _, s := range cfg.SelectedServices {
			if s == service {
				found = true
				break
			}
		}
		if !found {
			state = "disabled"
		}
		fmt.Printf("✓ %s %s\n", service, state)

	default:
		printConfigUsage()
	}
}

// ── interim instances ───────────────────────────────────────────────

// cmdInterim manages additional interim instances. interim is open source, so
// anyone can host one; each configured instance becomes its own service named
// interim@<label> that can be selected and uploaded to independently.
func cmdInterim() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	args := os.Args[2:]
	if len(args) == 0 {
		cmdInterimList(cfg)
		return
	}

	switch args[0] {
	case "list", "ls":
		cmdInterimList(cfg)
	case "add":
		cmdInterimAdd(cfg, args[1:])
	case "remove", "rm":
		cmdInterimRemove(cfg, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown interim command: %s\n\n", args[0])
		printInterimUsage()
		os.Exit(1)
	}
}

func printInterimUsage() {
	fmt.Print(`Manage self-hosted interim instances.

USAGE:
  manyup interim list
  manyup interim add <url> [name] [hash]
  manyup interim remove <name>

The default instance (https://interim.drifty.win) is always available as
service "interim". Every added instance is registered as "interim@<name>",
so it can be selected and uploaded to like any other service:

  manyup interim add https://files.example.com friends
  manyup config select interim@friends
  manyup upload myfile.zip

The user hash is optional. On an instance with public uploads enabled,
uploads work anonymously; on a restricted instance the hash must be approved
by that instance's admin. Omit the hash argument to be prompted; leave the
prompt empty for anonymous uploads.
`)
}

// looksLikeInterimHash distinguishes a user hash from an instance name in the
// optional positional argument. interim hashes are long and start with
// "interim_"; a label is limited to 48 characters.
func looksLikeInterimHash(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "interim_") && len(value) > 20
}

func cmdInterimAdd(cfg *config.AppConfig, args []string) {
	if len(args) < 1 || len(args) > 3 {
		fmt.Fprintln(os.Stderr, "Usage: manyup interim add <url> [name] [hash]")
		os.Exit(1)
	}

	origin, _, err := services.NormalizeInterimURL(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// The forms are: <url>, <url> <name>, <url> <name> <hash>. Treat a lone
	// second argument that looks like an interim hash as the hash, so
	// `interim add <url> interim_...` does the expected thing instead of
	// creating an instance named after the token.
	token := ""
	arguments := append([]string(nil), args[1:]...)
	if len(arguments) == 1 && looksLikeInterimHash(arguments[0]) {
		token = strings.TrimSpace(arguments[0])
		arguments = nil
	}
	if len(arguments) >= 2 {
		token = strings.TrimSpace(arguments[1])
	}

	name := ""
	if len(arguments) >= 1 {
		name = strings.ToLower(strings.TrimSpace(arguments[0]))
	}
	if name == "" {
		name = services.SuggestInterimInstanceName(origin)
	}
	if err := services.ValidateInterimInstanceName(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	service := services.InterimInstanceServiceName(name)
	if _, exists := cfg.Service(service); exists {
		fmt.Fprintf(os.Stderr, "Error: an interim instance named %q already exists (%s)\n", name, service)
		os.Exit(1)
	}

	// Refuse to configure the same origin twice: that would upload every file
	// to the same place twice.
	for _, candidate := range interimServices(cfg) {
		if candidate == services.InterimServiceName {
			continue
		}
		if services.ConfiguredInterimBaseURL(cfg, candidate) == origin {
			fmt.Fprintf(os.Stderr, "Error: %s is already configured as %s\n", origin, candidate)
			os.Exit(1)
		}
	}
	if services.ConfiguredInterimBaseURL(cfg, services.InterimServiceName) == origin {
		fmt.Fprintf(os.Stderr, "Error: %s is the default instance; set its hash with `manyup config set interim <hash>`\n", origin)
		os.Exit(1)
	}

	if token == "" {
		fmt.Printf("Paste an approved user hash for %s, or press Enter for anonymous uploads:\n> ", origin)
		token, err = readOptionalSecretValue(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
			os.Exit(1)
		}
	}

	instance := services.NewInterim(service, origin)
	note := ""
	if token != "" {
		note, err = authenticateCredential(instance, "API_TOKEN", token)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %v\n", err)
			fmt.Fprintln(os.Stderr, "Instance was not added.")
			os.Exit(1)
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), credentialTimeout)
		verifyErr := instance.VerifyCredentials(ctx, nil, nil)
		cancel()
		switch {
		case verifyErr == nil:
			note = "(anonymous uploads verified)"
		case errors.Is(verifyErr, plugin.ErrCredentialRejected):
			fmt.Fprintf(os.Stderr, "✗ %v\n", verifyErr)
			fmt.Fprintln(os.Stderr, "Instance was not added. Provide an approved user hash for a restricted instance.")
			os.Exit(1)
		default:
			note = fmt.Sprintf("(saved, but anonymous uploads could not be verified: %v)", verifyErr)
		}
	}

	cfg.SetOption(service, "BASE_URL", origin)
	if token != "" {
		cfg.SetCredential(service, "API_TOKEN", token)
	}
	cfg.SelectService(service)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✓ Added interim instance %s (%s) %s\n", name, origin, note)
	fmt.Printf("  service: %s (selected)\n", service)
}

func cmdInterimRemove(cfg *config.AppConfig, args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: manyup interim remove <name>")
		os.Exit(1)
	}

	name := strings.ToLower(strings.TrimSpace(args[0]))
	if name == "" || name == services.InterimServiceName {
		fmt.Fprintln(os.Stderr, "Error: the default interim instance cannot be removed; disable it with `manyup config select interim`")
		os.Exit(1)
	}
	if err := services.ValidateInterimInstanceName(name); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	service := services.InterimInstanceServiceName(name)
	_, exists := cfg.Service(service)
	if !exists && !cfg.IsSelected(service) {
		fmt.Fprintf(os.Stderr, "Error: no interim instance named %q (run `manyup interim list`)\n", name)
		os.Exit(1)
	}

	cfg.RemoveService(service)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Removed interim instance %s (%s)\n", name, service)
}

func cmdInterimList(cfg *config.AppConfig) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tURL\tAUTH\tSELECTED")
	fmt.Fprintln(w, "-------\t---\t----\t--------")
	for _, service := range interimServices(cfg) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			service,
			services.ConfiguredInterimBaseURL(cfg, service),
			interimAuthState(cfg, service),
			yesNo(cfg.IsSelected(service)))
	}
	w.Flush()
	fmt.Println()
	fmt.Println("Add another instance:  manyup interim add https://files.example.com [name]")
}

// interimServices lists the default instance followed by configured instances.
func interimServices(cfg *config.AppConfig) []string {
	servicesList := []string{services.InterimServiceName}
	var instances []string
	for service := range cfg.Services {
		if _, ok := services.InterimInstanceName(service); ok {
			instances = append(instances, service)
		}
	}
	sort.Strings(instances)
	return append(servicesList, instances...)
}

// interimAuthState describes where a credential would come from, if anywhere.
func interimAuthState(cfg *config.AppConfig, service string) string {
	if sc, ok := cfg.Service(service); ok && sc.Credentials != nil {
		if strings.TrimSpace(sc.Credentials["API_TOKEN"]) != "" {
			return "hash"
		}
	}
	if service == services.InterimServiceName {
		env := "MANYUP_" + strings.ToUpper(service) + "_API_TOKEN"
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return "hash (env)"
		}
	}
	return "anonymous"
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func printConfigUsage() {
	fmt.Print(`Usage: manyup config <command>

Commands:
  menu                        Interactive service selector
  set <service>               Prompt for the token (safe for any characters)
  set <service> <value>       Set the API token (key defaults to API_TOKEN)
  set <service> <key> <value> Set any credential or option explicitly
  show                        Show current configuration
  mode <parallel|sequential>  Set upload mode
  select <service>            Toggle a service on/off
  retry <max> [seconds]       Retries per service + base backoff delay (0 disables)

Credentials are checked against the service before they are saved.

If a token contains shell characters such as | & < > ^, quote it or omit the
value and paste it at the prompt — the shell strips those before manyup runs.

Self-hosted interim instances are added with "manyup interim add <url> [name]"
and then selected like any other service (for example "interim@friends").
`)
}

// parseSetArgs accepts three forms of `config set`:
//
//	manyup config set <service>                  value is prompted for
//	manyup config set <service> <value>          key defaults to API_TOKEN
//	manyup config set <service> <key> <value>
//
// A value of "" is returned when it was omitted, so the caller can read it from
// stdin; an explicitly empty argument is still an error.
func parseSetArgs(args []string) (service, key, value string, err error) {
	switch {
	case len(args) < 1:
		return "", "", "", errors.New("missing service")
	case len(args) > 3:
		return "", "", "", errors.New("too many arguments")
	}

	service = args[0]
	switch len(args) {
	case 1:
		key = "API_TOKEN" // value will be prompted for
	case 2:
		key, value = "API_TOKEN", args[1]
	default:
		key, value = args[1], args[2]
	}

	if len(args) >= 2 && value == "" {
		return "", "", "", fmt.Errorf("the value for %s/%s cannot be empty", service, key)
	}
	return service, key, value, nil
}

// readSecretValue reads one line from r. It is used when a credential is typed
// at a prompt instead of passed as an argument: values such as API tokens often
// contain characters the shell would consume (| & < > ^), and those never reach
// the process at all when they are part of a command-line argument.
func readSecretValue(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no value provided")
		}
		return "", fmt.Errorf("reading input: %w", err)
	}

	value := strings.TrimSpace(line)
	if value == "" {
		return "", errors.New("no value provided")
	}
	return value, nil
}

// readOptionalSecretValue reads one line from r and allows it to be empty. It is
// used where a credential is genuinely optional, such as an interim instance
// with public uploads enabled: pressing Enter means "no hash".
func readOptionalSecretValue(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// isCredentialKey reports whether a config key identifies the caller to a
// service. Options such as a folder id are not credentials and are not checked.
func isCredentialKey(key string) bool {
	switch key {
	case "API_TOKEN", "API_KEY", "TOKEN":
		return true
	}
	return false
}

// authenticateCredential verifies a credential against the service before the
// CLI persists it, and returns a short note for the success message.
//
// A credential the service explicitly rejects is returned as an error so the
// caller can abort without saving. When the check cannot be completed — the
// host is unreachable, rate limiting, or the service has no verifier — the
// credential is still saved and the note says so, because refusing would make
// configuration impossible whenever a host is down.
func authenticateCredential(svc plugin.Uploader, key, value string) (string, error) {
	if !isCredentialKey(key) {
		return "", nil
	}

	verifier, ok := svc.(plugin.CredentialVerifier)
	if !ok {
		return "(saved without verification — this service has no credential check)", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), credentialTimeout)
	defer cancel()

	err := verifier.VerifyCredentials(ctx, plugin.Credentials{key: value}, nil)
	switch {
	case err == nil:
		return "(verified)", nil
	case errors.Is(err, plugin.ErrCredentialRejected):
		return "", err
	default:
		return fmt.Sprintf("(saved, but it could not be verified: %v)", err), nil
	}
}

// ── Interactive config menu ─────────────────────────────────────────

func cmdConfigMenu() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	registry := services.AllWithConfig(cfg)
	names := registry.Names()

	// Build selection set from current config.
	selected := make(map[string]bool)
	for _, s := range cfg.SelectedServices {
		selected[s] = true
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		// Clear screen and draw menu.
		fmt.Print("\033[2J\033[H")
		fmt.Println("manyup — Select upload services")
		fmt.Println(strings.Repeat("─", 40))
		fmt.Println()

		for i, name := range names {
			svc, _ := registry.Get(name)
			marker := " "
			if selected[name] {
				marker = "x"
			}
			fmt.Printf("  %d. [%s] %-14s %s\n", i+1, marker, svc.DisplayName(), svc.Description())
		}

		fmt.Println()
		fmt.Printf("  Upload mode: %s\n", cfg.UploadMode)
		maxRetries, delay := cfg.RetrySettings()
		fmt.Printf("  Retries: %d per service (%ds base delay)\n", maxRetries, delay)
		fmt.Println()
		fmt.Println("  Commands:")
		fmt.Println("    <number>  Toggle service")
		fmt.Println("    a         Select all")
		fmt.Println("    n         Deselect all")
		fmt.Println("    m         Toggle parallel/sequential")
		fmt.Println("    Enter     Save and exit")
		fmt.Println()
		fmt.Print("  > ")

		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)

		if line == "" {
			break
		}

		switch {
		case line == "a":
			for _, name := range names {
				selected[name] = true
			}
		case line == "n":
			for _, name := range names {
				selected[name] = false
			}
		case line == "m":
			if cfg.UploadMode == config.ModeParallel {
				cfg.UploadMode = config.ModeSequential
			} else {
				cfg.UploadMode = config.ModeParallel
			}
		default:
			// Try to parse as a number.
			var idx int
			if _, scanErr := fmt.Sscanf(line, "%d", &idx); scanErr == nil && idx >= 1 && idx <= len(names) {
				name := names[idx-1]
				selected[name] = !selected[name]
			}
		}
	}

	// Save selection.
	cfg.SelectedServices = nil
	for _, name := range names {
		if selected[name] {
			cfg.SelectedServices = append(cfg.SelectedServices, name)
		}
	}

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
		os.Exit(1)
	}

	fmt.Print("\033[2J\033[H")
	fmt.Printf("✓ Saved. Services: %s\n", strings.Join(cfg.SelectedServices, ", "))
}

func printConfig(cfg *config.AppConfig) {
	maxRetries, delay := cfg.RetrySettings()
	fmt.Printf("Upload mode: %s\n", cfg.UploadMode)
	fmt.Printf("Retries: %d per service (base delay %ds)\n", maxRetries, delay)
	fmt.Printf("Selected services: %s\n", strings.Join(cfg.SelectedServices, ", "))
	fmt.Println("\nService credentials:")

	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc := cfg.Services[name]
		keys := make([]string, 0, len(sc.Credentials))
		for k := range sc.Credentials {
			keys = append(keys, k)
		}
		detail := ""
		if url := services.ConfiguredInterimBaseURL(cfg, name); url != "" {
			detail = " [" + url + "]"
		}
		fmt.Printf("  %s%s: %s (enabled: %v)\n", name, detail, strings.Join(keys, ", "), sc.Enabled)
	}
}

// ── Progress bar display ────────────────────────────────────────────────

type serviceBar struct {
	name     string
	bytes    int64
	total    int64
	percent  float64
	speed    float64
	status   string // "", "retry", "done", "error"
	retryMsg string // shown while waiting to retry
}

type progressDisplay struct {
	mu        sync.Mutex
	bars      []*serviceBar
	barIndex  map[string]int
	lineCount int
}

func newProgressDisplay() *progressDisplay {
	return &progressDisplay{barIndex: make(map[string]int)}
}

func (d *progressDisplay) handle(p uploader.Progress) {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch p.State {
	case "starting":
		// If the previous batch finished, reset so bars start fresh.
		if d.batchDone() {
			d.bars = nil
			d.barIndex = make(map[string]int)
			d.lineCount = 0
		}
		bar := &serviceBar{name: p.Service}
		idx := len(d.bars)
		d.bars = append(d.bars, bar)
		d.barIndex[p.Service] = idx
		d.lineCount++
		if idx > 0 {
			fmt.Print("\n")
		}
		fmt.Print(formatBarLine(bar))

	case "uploading":
		idx, ok := d.barIndex[p.Service]
		if !ok {
			return
		}
		bar := d.bars[idx]
		bar.status = "" // clears a previous retry note
		bar.retryMsg = ""
		bar.bytes = p.Bytes
		bar.total = p.Total
		bar.percent = p.Percent
		bar.speed = p.Speed
		d.renderAt(bar, idx)

	case "retry":
		idx, ok := d.barIndex[p.Service]
		if !ok {
			return
		}
		bar := d.bars[idx]
		bar.status = "retry"
		bar.bytes = 0
		bar.total = p.Total
		bar.percent = 0
		bar.speed = 0
		bar.retryMsg = fmt.Sprintf("retry %d/%d · %s", p.Attempt, p.MaxAttempts, p.Message)
		d.renderAt(bar, idx)

	case "done":
		idx, ok := d.barIndex[p.Service]
		if !ok {
			return
		}
		d.bars[idx].status = "done"
		d.renderAt(d.bars[idx], idx)

	case "error":
		idx, ok := d.barIndex[p.Service]
		if !ok {
			return
		}
		d.bars[idx].status = "error"
		d.renderAt(d.bars[idx], idx)
	}
}

// renderAt redraws a single bar line using ANSI cursor movement.
func (d *progressDisplay) renderAt(bar *serviceBar, idx int) {
	currentLine := d.lineCount - 1
	up := currentLine - idx
	if up > 0 {
		fmt.Printf("\033[%dA", up)
	}
	fmt.Print("\r\033[K")
	fmt.Print(formatBarLine(bar))
	if up > 0 {
		fmt.Printf("\033[%dB", up)
	}
}

func (d *progressDisplay) batchDone() bool {
	if len(d.bars) == 0 {
		return false
	}
	for _, b := range d.bars {
		if b.status != "done" && b.status != "error" {
			return false
		}
	}
	return true
}

// barNameWidth keeps progress columns aligned even when an interim instance
// service is named interim@files.example.com.
const barNameWidth = 20

// barName truncates a long service name so a progress row does not overflow.
func barName(name string) string {
	runes := []rune(name)
	if len(runes) <= barNameWidth {
		return name
	}
	return string(runes[:barNameWidth-1]) + "…"
}

func formatBarLine(bar *serviceBar) string {
	if bar.status == "done" {
		return fmt.Sprintf("  %-*s ✓ complete", barNameWidth, barName(bar.name))
	}
	if bar.status == "error" {
		return fmt.Sprintf("  %-*s ✗ failed", barNameWidth, barName(bar.name))
	}
	if bar.status == "retry" {
		return fmt.Sprintf("  %-*s ↻ %s", barNameWidth, barName(bar.name), bar.retryMsg)
	}

	const barWidth = 20
	filled := int(bar.percent / 100 * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}
	empty := barWidth - filled
	barStr := strings.Repeat("█", filled) + strings.Repeat("░", empty)

	pctStr := fmt.Sprintf("%5.1f%%", bar.percent)

	speedStr := "      -"
	if bar.speed > 0 {
		speedStr = fmt.Sprintf("%7s", formatBytes(bar.speed)+"/s")
	}

	etaStr := "-"
	if bar.speed > 0 && bar.total > 0 && bar.bytes < bar.total {
		remaining := float64(bar.total-bar.bytes) / bar.speed
		eta := time.Duration(remaining * float64(time.Second))
		etaStr = formatDuration(eta)
	}

	return fmt.Sprintf("  %-*s %s  %s  %s  ETA %s", barNameWidth, barName(bar.name), barStr, pctStr, speedStr, etaStr)
}

func formatBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %02ds", m, s)
}

func printResult(result *uploader.MultiResult) {
	fmt.Println(strings.Repeat("─", 60))
	for _, r := range result.Results {
		if r.Error != nil {
			fmt.Printf("  ✗ %s: %v\n", r.Service, r.Error)
		} else {
			fmt.Printf("  ✓ %s: %s (%.1fs)\n", r.Service, r.URL, r.Duration)
		}
	}
	fmt.Printf("  Total time: %.1fs\n", result.TotalTime)
	fmt.Println(strings.Repeat("─", 60))
}

// ── Platform detection ──────────────────────────────────────────────────

func detectPlatform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// ── Update command ──────────────────────────────────────────────────────

func cmdUpdate() {
	fmt.Println("Checking for updates...")
	platform := detectPlatform()

	latest, err := update.CheckLatest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking for updates: %v\n", err)
		os.Exit(1)
	}

	// A build from source may carry a version that is not X.Y.Z (for example
	// "dev"). Treat that as older than any release rather than refusing to
	// update at all.
	current, parseErr := update.ParseSemVer(version)
	if parseErr == nil && !latest.Version.IsNewer(current) {
		fmt.Printf("✓ manyup %s is already the latest version.\n", current.String())
		return
	}

	shown := version
	if parseErr == nil {
		shown = current.String()
	}

	assetURL, err := latest.FindAsset(platform)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Current version: %s\n", shown)
	fmt.Printf("Latest version:  %s\n", latest.TagName)
	fmt.Printf("Platform:        %s\n", platform)
	fmt.Printf("\nDownloading %s...\n", latest.TagName)

	if err := update.Apply(assetURL); err != nil {
		fmt.Fprintf(os.Stderr, "\nError updating: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n✓ Updated to %s. The next time you run manyup it will be the new version.\n", latest.TagName)
}

// ── Shell completion ────────────────────────────────────────────────────
//
// Completion is split in two: this program prints candidate words, and a small
// shell script (printed by `manyup completion <shell>`) asks for them on tab.
// Nothing happens until the script is sourced into the user's shell profile.

// topLevelCommands are offered when completing the first word.
var topLevelCommands = []string{
	"upload", "services", "config", "interim", "completion", "where", "version", "update", "help",
}

// configCommands are offered after `manyup config`.
var configCommands = []string{"menu", "set", "show", "mode", "select", "retry"}

// interimCommands are offered after `manyup interim`.
var interimCommands = []string{"list", "add", "remove"}

// shells are the completion scripts this build can emit.
var shells = []string{"bash", "zsh", "fish", "powershell", "clink"}

// clinkCompletion is the cmd.exe (Clink) script. It is embedded so the binary
// can print it without shipping a separate file next to the executable.
//
//go:embed scripts/clink/manyup.lua
var clinkCompletion string

// cmdComplete prints the candidates for the words already typed. It is the
// hidden backend used by the generated completion scripts.
func cmdComplete() {
	registry := services.All()
	if cfg, err := config.Load(); err == nil {
		registry = services.AllWithConfig(cfg)
	}
	for _, candidate := range completionCandidatesFor(os.Args[2:], registry) {
		fmt.Println(candidate)
	}
}

// completionCandidates maps the words typed so far (everything after "manyup",
// excluding the word being completed) to the words that may follow. It uses the
// built-in services only; the CLI uses completionCandidatesFor so configured
// interim instances are offered too.
func completionCandidates(words []string) []string {
	return completionCandidatesFor(words, services.All())
}

func completionCandidatesFor(words []string, registry *plugin.Registry) []string {
	if len(words) == 0 {
		return topLevelCommands
	}

	switch words[0] {
	case "upload":
		return uploadSourceCandidates(words[1:], registry)
	case "config":
		switch len(words) {
		case 1:
			return configCommands
		case 2:
			switch words[1] {
			case "set", "select":
				return registry.Names()
			case "mode":
				return []string{"parallel", "sequential"}
			}
		}
	case "interim":
		switch len(words) {
		case 1:
			return interimCommands
		case 2:
			switch words[1] {
			case "remove", "rm":
				return interimInstanceLabels(registry)
			}
		}
	case "completion":
		if len(words) == 1 {
			return shells
		}
	}
	return nil
}

// uploadSourceCandidates completes service names after -s/--sources. It stops
// once a token is no longer a service name, so a file after the source list is
// not misread as another source.
func uploadSourceCandidates(args []string, registry *plugin.Registry) []string {
	sourceIndex := -1
	for i, arg := range args {
		switch {
		case arg == "-s" || arg == "--sources":
			sourceIndex = i
		case strings.HasPrefix(arg, "-s=") || strings.HasPrefix(arg, "--sources="):
			// The whole list was supplied inline; nothing more to complete.
			return nil
		}
	}
	if sourceIndex < 0 {
		return nil
	}

	known := make(map[string]string, len(registry.Names()))
	for _, name := range registry.Names() {
		known[strings.ToLower(name)] = name
	}

	used := make(map[string]bool)
	for _, arg := range args[sourceIndex+1:] {
		canonical, ok := known[strings.ToLower(arg)]
		if !ok {
			return nil
		}
		used[canonical] = true
	}

	out := make([]string, 0, len(known))
	for _, name := range registry.Names() {
		if !used[name] {
			out = append(out, name)
		}
	}
	return out
}

// interimInstanceLabels lists configured instances without the interim@ prefix,
// for `manyup interim remove <TAB>`.
func interimInstanceLabels(registry *plugin.Registry) []string {
	var labels []string
	for _, service := range registry.Names() {
		if label, ok := services.InterimInstanceName(service); ok {
			labels = append(labels, label)
		}
	}
	return labels
}

// cmdCompletion prints the completion script for a shell.
func cmdCompletion() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: manyup completion <%s>\n", strings.Join(shells, "|"))
		os.Exit(1)
	}

	script, ok := completionScript(os.Args[2])
	if !ok {
		fmt.Fprintf(os.Stderr, "Unsupported shell: %s (supported: %s)\n",
			os.Args[2], strings.Join(shells, ", "))
		os.Exit(1)
	}
	fmt.Print(script)
}

func completionScript(shell string) (string, bool) {
	switch shell {
	case "bash":
		return bashCompletion, true
	case "zsh":
		return zshCompletion, true
	case "fish":
		return fishCompletion, true
	case "powershell", "pwsh":
		return powershellCompletion, true
	case "clink":
		return clinkCompletion, true
	}
	return "", false
}

const bashCompletion = `# manyup completion for bash — add this line to ~/.bashrc:
#   eval "$(manyup completion bash)"
_manyup_completions() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local args=()
    local i
    for ((i = 1; i < COMP_CWORD; i++)); do
        args+=("${COMP_WORDS[i]}")
    done
    local candidates
    candidates="$(manyup __complete "${args[@]}" 2>/dev/null)"
    COMPREPLY=($(compgen -W "${candidates}" -- "${cur}"))
}
complete -o default -F _manyup_completions manyup
`

const zshCompletion = `# manyup completion for zsh — add this line to ~/.zshrc:
#   eval "$(manyup completion zsh)"
_manyup() {
    local -a args candidates
    args=("${words[2,$((CURRENT - 1))]}")
    candidates=("${(@f)$(manyup __complete "${args[@]}" 2>/dev/null)}")
    compadd -a candidates
}
compdef _manyup manyup
`

const fishCompletion = `# manyup completion for fish — add this line to ~/.config/fish/config.fish:
#   manyup completion fish | source
complete -c manyup -f -a '(manyup __complete (commandline -opc)[2..-1] 2>/dev/null)'
`

const powershellCompletion = `# manyup completion for PowerShell — add this line to your $PROFILE:
#   manyup completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -Native -CommandName manyup -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.ToString() })
    if ($words.Count -gt 0 -and $words[-1] -eq $wordToComplete) {
        $words = @($words | Select-Object -SkipLast 1)
    }
    manyup __complete @words 2>$null | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`
