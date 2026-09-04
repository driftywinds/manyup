package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/multiuploader/manyup/internal/config"
	"github.com/multiuploader/manyup/internal/services"
	"github.com/multiuploader/manyup/internal/uploader"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}

	switch os.Args[1] {
	case "upload":
		cmdUpload()
	case "services":
		cmdServices()
	case "config":
		cmdConfig()
	case "version":
		fmt.Printf("manyup %s\n", version)
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
  services                    List available service plugins
  config menu                 Interactive service selector
  config set <service> <key> <value>   Set a credential
  config show                 Show current configuration
  config mode <parallel|sequential>    Set upload mode
  config select <service>     Toggle a service on/off
  version                     Print version

EXAMPLES:
  # Interactive service selector
  manyup config menu

  # Configure a service
  manyup config set gofile API_KEY mytoken123

  # Upload a file
  manyup upload myfile.zip

  # Upload multiple files
  manyup upload *.zip

ENVIRONMENT VARIABLES:
  Credentials can also be set via env vars:
    MANYUP_<SERVICE>_<KEY>  e.g. MANYUP_GOFILE_API_KEY=xxx
`)
}

func cmdUpload() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: manyup upload <file> [file...]")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	if len(cfg.SelectedServices) == 0 {
		fmt.Fprintln(os.Stderr, "No services selected. Use 'manyup config select <service>' first.")
		os.Exit(1)
	}

	registry := services.All()
	mgr := uploader.New(registry, cfg)

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

	files := os.Args[2:]
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
	registry := services.All()
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
		if len(args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: manyup config set <service> <key> <value>")
			os.Exit(1)
		}
		service, key, value := args[1], args[2], args[3]
		cfg.SetCredential(service, key, value)
		if err := cfg.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✓ Set %s/%s\n", service, key)

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

	case "select":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: manyup config select <service>")
			os.Exit(1)
		}
		service := args[1]
		registry := services.All()
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

func printConfigUsage() {
	fmt.Print(`Usage: manyup config <command>

Commands:
  menu                          Interactive service selector
  set <service> <key> <value>   Set a credential for a service
  show                          Show current configuration
  mode <parallel|sequential>    Set upload mode
  select <service>              Toggle a service on/off
`)
}

// ── Interactive config menu ─────────────────────────────────────────

func cmdConfigMenu() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	registry := services.All()
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

		for i, name := range registry.Names() {
		svc, _ := registry.Get(name)
		marker := " "
		if selected[name] {
			marker = "x"
		}
		fmt.Printf("  %d. [%s] %-14s %s\n", i+1, marker, svc.DisplayName(), svc.Description())
		}

		fmt.Println()
		fmt.Printf("  Upload mode: %s\n", cfg.UploadMode)
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
	fmt.Printf("Upload mode: %s\n", cfg.UploadMode)
	fmt.Printf("Selected services: %s\n", strings.Join(cfg.SelectedServices, ", "))
	fmt.Println("\nService credentials:")
	for name, sc := range cfg.Services {
		keys := make([]string, 0, len(sc.Credentials))
		for k := range sc.Credentials {
			keys = append(keys, k)
		}
		fmt.Printf("  %s: %s (enabled: %v)\n", name, strings.Join(keys, ", "), sc.Enabled)
	}
}

// ── Progress bar display ────────────────────────────────────────────────

type serviceBar struct {
	name    string
	bytes   int64
	total   int64
	percent float64
	speed   float64
	status  string // "", "done", "error"
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
		bar.bytes = p.Bytes
		bar.total = p.Total
		bar.percent = p.Percent
		bar.speed = p.Speed
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

func formatBarLine(bar *serviceBar) string {
	if bar.status == "done" {
		return fmt.Sprintf("  %-12s ✓ complete", bar.name)
	}
	if bar.status == "error" {
		return fmt.Sprintf("  %-12s ✗ failed", bar.name)
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

	return fmt.Sprintf("  %-12s %s  %s  %s  ETA %s", bar.name, barStr, pctStr, speedStr, etaStr)
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
