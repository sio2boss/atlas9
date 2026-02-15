// atlas9 — TUI for Atlas workflow (Inspect → Lint → Preview → Apply).
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/fsnotify/fsnotify"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/docopt/docopt-go"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// overlayRoot draws content full-screen and optionally an overlay primitive (e.g. modal) on top.
// overlay is a pointer so it can be set to nil when the modal closes.
type overlayRoot struct {
	*tview.Box
	content tview.Primitive
	overlay *tview.Primitive
}

func newOverlayRoot(content tview.Primitive, overlay *tview.Primitive) *overlayRoot {
	return &overlayRoot{
		Box:     tview.NewBox(),
		content: content,
		overlay: overlay,
	}
}

func (o *overlayRoot) SetRect(x, y, width, height int) {
	o.Box.SetRect(x, y, width, height)
	o.content.SetRect(x, y, width, height)
}

func (o *overlayRoot) Draw(screen tcell.Screen) {
	o.content.Draw(screen)
	if o.overlay != nil && *o.overlay != nil {
		(*o.overlay).Draw(screen)
	}
}

func (o *overlayRoot) InputHandler() func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
	return func(event *tcell.EventKey, setFocus func(p tview.Primitive)) {
		if o.overlay != nil && *o.overlay != nil {
			if h := (*o.overlay).InputHandler(); h != nil {
				h(event, setFocus)
				return
			}
		}
		if h := o.content.InputHandler(); h != nil {
			h(event, setFocus)
		}
	}
}

func (o *overlayRoot) Focus(delegate func(p tview.Primitive)) {
	if o.overlay != nil && *o.overlay != nil {
		delegate(*o.overlay)
	} else {
		delegate(o.content)
	}
}

func (o *overlayRoot) HasFocus() bool {
	if o.overlay != nil && *o.overlay != nil {
		return (*o.overlay).HasFocus()
	}
	return o.content.HasFocus()
}

const (
	logoColorHex = "#98E0EA" // light cyan/teal
	version      = "v0.9.0"  // updated by Makefile update-version
)

const usageDoc = `atlas9 — TUI for Atlas workflow.

Usage:
  atlas9 [options]

Options:
  -h, --help          Show this help.
  -v, --version       Show version.
  -e, --env <env>     Override environment (default: from .env ENVIRONMENT or local)`

// High ASCII block-art "atlas9" (4 lines) + tagline.
const logoAtlas9 = `   ▐  ▜       ▞▀▖
▝▀▖▜▀ ▐ ▝▀▖▞▀▘▚▄▌
▞▀▌▐ ▖▐ ▞▀▌▝▀▖▖ ▌
▝▀▘ ▀  ▘▝▀▘▀▀ ▝▀ 
manage your database schema as code...`

var stages = []string{"Status", "Diff", "Lint", "Dry-Run", "Apply"}
var stageDescriptions = []string{
	"Show applied vs pending",
	"Generate migration file",
	"Hash + safety checks",
	"Preview pending SQL",
	"Apply pending changes",
}

// parseEnvFile reads a .env file (KEY=VALUE per line) and returns a map. Returns nil map on error (e.g. file not found).
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// loadEnv reads path as .env and updates overrides (guarded by envMu). Call from watcher or once at start of watcher goroutine.
func loadEnv(path string, overrides map[string]string, envMu *sync.Mutex) {
	parsed, _ := parseEnvFile(path)
	envMu.Lock()
	defer envMu.Unlock()
	for k := range overrides {
		delete(overrides, k)
	}
	for k, v := range parsed {
		overrides[k] = v
	}
}

// parseAtlasHCLEnvs reads atlas.hcl and returns the names of env blocks (e.g. ["localdev", "dev", "prod"]).
func parseAtlasHCLEnvs(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var names []string
	s := string(data)
	const prefix = `env "`
	for {
		i := strings.Index(s, prefix)
		if i < 0 {
			break
		}
		s = s[i+len(prefix):]
		end := strings.Index(s, `"`)
		if end < 0 {
			break
		}
		names = append(names, s[:end])
		s = s[end+1:]
	}
	return names
}

// parseDiffSummary parses SQL diff output and returns a git-like summary.
// Returns lines like "+++ users (CREATE TABLE)" or "--- old_table (DROP TABLE)" or "~~~ posts (ALTER TABLE)"
func parseDiffSummary(sql string) string {
	var lines []string
	var creates, drops, alters []string

	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		upper := strings.ToUpper(trimmed)

		// CREATE TABLE
		if strings.HasPrefix(upper, "CREATE TABLE") {
			// Extract table name: CREATE TABLE "tablename" or CREATE TABLE tablename
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				tableName := strings.Trim(parts[2], "\"(`")
				creates = append(creates, tableName)
			}
		}
		// DROP TABLE
		if strings.HasPrefix(upper, "DROP TABLE") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				tableName := strings.Trim(parts[2], "\"(`")
				drops = append(drops, tableName)
			}
		}
		// ALTER TABLE
		if strings.HasPrefix(upper, "ALTER TABLE") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				tableName := strings.Trim(parts[2], "\"(`")
				// Avoid duplicates
				found := false
				for _, t := range alters {
					if t == tableName {
						found = true
						break
					}
				}
				if !found {
					alters = append(alters, tableName)
				}
			}
		}
	}

	// Build summary
	for _, t := range creates {
		lines = append(lines, fmt.Sprintf("[green]+++ %s[-]  (CREATE TABLE)", t))
	}
	for _, t := range alters {
		lines = append(lines, fmt.Sprintf("[yellow]~~~ %s[-]  (ALTER TABLE)", t))
	}
	for _, t := range drops {
		lines = append(lines, fmt.Sprintf("[red]--- %s[-]  (DROP TABLE)", t))
	}

	if len(lines) == 0 {
		return "[green]No schema changes detected.[-]"
	}

	return strings.Join(lines, "\n")
}

func highlightWithLexer(lexerName, text string) string {
	lexer := lexers.Get(lexerName)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	style := styles.Get("monokai")
	if style == nil {
		style = styles.Fallback
	}
	formatter := formatters.Get("terminal256")
	if formatter == nil {
		formatter = formatters.Fallback
	}
	iterator, err := lexer.Tokenise(nil, text)
	if err != nil {
		return text
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iterator); err != nil {
		return text
	}
	return buf.String()
}

// highlightSQL returns SQL with ANSI color codes for terminal display.
func highlightSQL(sql string) string {
	return highlightWithLexer("sql", sql)
}

// highlightHCL returns HCL (atlas.hcl) with ANSI color codes for terminal display.
func highlightHCL(hcl string) string {
	return highlightWithLexer("hcl", hcl)
}

// visiblePosition returns the index in highlighted (which may contain ANSI codes) where
// the nth visible character (0-based) appears. Used to insert a cursor marker.
func visiblePosition(highlighted string, n int) int {
	inEscape := false
	bracket := false
	visible := 0
	for i, r := range highlighted {
		if inEscape {
			if r == 'm' || r == ']' {
				inEscape = false
				bracket = false
			}
			continue
		}
		if bracket && r == '[' {
			continue
		}
		if r == '\x1b' {
			inEscape = true
			bracket = (i+1 < len(highlighted) && highlighted[i+1] == '[')
			continue
		}
		if r == '[' && i > 0 && highlighted[i-1] == '\x1b' {
			continue
		}
		visible++
		if visible > n {
			return i
		}
	}
	return len(highlighted)
}

func main() {
	workDir, _ := os.Getwd()
	envPath := filepath.Join(workDir, ".env")
	atlasHCL := filepath.Join(workDir, "atlas.hcl")

	opts, err := docopt.ParseArgs(usageDoc, os.Args[1:], version)
	if err != nil {
		fmt.Fprintln(os.Stderr, usageDoc)
		os.Exit(1)
	}
	if ok, _ := opts.Bool("--version"); ok {
		fmt.Println(version)
		os.Exit(0)
	}
	if ok, _ := opts.Bool("--help"); ok {
		fmt.Println(usageDoc)
		os.Exit(0)
	}

	// In-memory env overlay from .env (updated by watcher); all env reads go through getEnv so UI and atlas see .env values.
	var envOverrides = make(map[string]string)
	var envMu sync.Mutex
	getEnv := func(key string) string {
		envMu.Lock()
		v, ok := envOverrides[key]
		envMu.Unlock()
		if ok {
			return v
		}
		return os.Getenv(key)
	}
	// Current environment: --env flag overrides, then .env overlay (ENVIRONMENT), then process, then "local"
	getCurrentEnvName := func() string {
		if e, _ := opts.String("--env"); e != "" {
			return e
		}
		if v := getEnv("ENVIRONMENT"); v != "" {
			return v
		}
		return "local"
	}

	// Use terminal's native background color (don't draw any background)
	tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault
	tview.Styles.ContrastBackgroundColor = tcell.ColorDefault
	tview.Styles.MoreContrastBackgroundColor = tcell.ColorDefault

	app := tview.NewApplication()
	logoColor := hexToTCell(logoColorHex)

	// Use single-line borders when a box has focus (output box and modals).
	tview.Borders.HorizontalFocus = tview.BoxDrawingsLightHorizontal
	tview.Borders.VerticalFocus = tview.BoxDrawingsLightVertical
	tview.Borders.TopLeftFocus = tview.BoxDrawingsLightDownAndRight
	tview.Borders.TopRightFocus = tview.BoxDrawingsLightDownAndLeft
	tview.Borders.BottomLeftFocus = tview.BoxDrawingsLightUpAndRight
	tview.Borders.BottomRightFocus = tview.BoxDrawingsLightUpAndLeft

	// State
	var (
		stageIndex    int
		dockerOK      bool
		atlasLoggedIn bool
		statusMu      sync.Mutex
		running       bool
		inOverlay     bool // true when config/modal/preview is showing (Esc closes it instead of quitting)
		editMode      bool // true when editing the command line (vim-like: 'i' to enter, Esc to exit)
	)

	// Logo (top left)
	logoView := tview.NewTextView().
		SetText(logoAtlas9).
		SetTextColor(logoColor).
		SetDynamicColors(false)
	logoView.SetBorder(false)
	// Top right: docker, atlas.hcl env match, env name (from .env ENVIRONMENT), APP_DB_URL (from .env or process)
	topRightView := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	topRightView.SetBorder(false)
	updateTopRight := func() {
		statusMu.Lock()
		dockerStatus := dockerOK
		statusMu.Unlock()

		currentEnvName := getCurrentEnvName()
		atlasEnvs := parseAtlasHCLEnvs(atlasHCL)
		hasAtlasEnv := false
		for _, n := range atlasEnvs {
			if n == currentEnvName {
				hasAtlasEnv = true
				break
			}
		}
		appDBURLSet := getEnv("APP_DB_URL") != ""

		var dockerStr string
		if dockerStatus {
			dockerStr = "docker  [green]✅[-]"
		} else {
			dockerStr = "docker  [red]❌[-]"
		}
		var atlasHCLStr string
		if hasAtlasEnv {
			atlasHCLStr = fmt.Sprintf("atlas.hcl: %s  [green]✅[-]", currentEnvName)
		} else {
			atlasHCLStr = fmt.Sprintf("atlas.hcl: %s  [red]❌[-]", currentEnvName)
		}
		envStr := fmt.Sprintf("env: %s  [green]✅[-]", currentEnvName)
		var appDBStr string
		if appDBURLSet {
			appDBStr = "APP_DB_URL  [green]✅[-]"
		} else {
			appDBStr = "APP_DB_URL  [red]❌[-]"
		}
		topRightView.SetText(dockerStr + "\n" + atlasHCLStr + "\n" + envStr + "\n" + appDBStr)
	}
	updateTopRight()

	// Top row: logo left, docker+env right (wide enough for APP_DB_URL on one line)
	topFlex := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(logoView, 0, 1, false).
		AddItem(topRightView, 28, 0, false)
	// Stage strip: single row of text with arrows; current stage in atlas blue + bold
	stageRowView := tview.NewTextView().SetDynamicColors(true)
	buildStageRowText := func(highlightIdx int, underline bool) string {
		var parts []string
		for i, name := range stages {
			if i == highlightIdx {
				// Only the selected stage name gets highlight (blue+bold) and optionally underline.
				// Explicitly turn off bold (B) and underline (U) after the word so the rest of the line stays plain.
				seg := "[#98E0EA::b]"
				if underline {
					seg += "[::u]" + name + "[::BU][-]"
				} else {
					seg += name + "[::B][-]"
				}
				parts = append(parts, seg)
			} else {
				parts = append(parts, name)
			}
		}
		return strings.Join(parts, " → ")
	}
	stageRowView.SetText(buildStageRowText(0, true))
	stageRowView.SetBorder(false)
	const stripIndent = 4
	stripIndentView := tview.NewTextView().SetText("")
	stripIndentView.SetBorder(false)
	stageStripRow := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(stripIndentView, stripIndent, 0, false).
		AddItem(stageRowView, 0, 1, true) // focusable so Down moves to body
	spacerBelowStages := tview.NewTextView().SetText("")
	spacerBelowStages.SetBorder(false)
	// isLintAvailable returns true if Lint stage should be active
	isLintAvailable := func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		return atlasLoggedIn
	}

	// projectedCommand returns the exact atlas command for the given stage and env.
	projectedCommand := func(stageIdx int, env string) string {
		switch stageIdx {
		case 0:
			return "atlas migrate status --env " + env
		case 1:
			return "atlas migrate diff --env " + env
		case 2:
			return "atlas migrate hash --env " + env + " && atlas migrate lint --env " + env
		case 3:
			return "atlas migrate apply --env " + env + " --dry-run"
		case 4:
			return "atlas migrate apply --env " + env
		default:
			return "atlas"
		}
	}

	// Body: description (first line) + "> " command input + scrollable output
	descriptionView := tview.NewTextView().SetDynamicColors(true)
	descriptionView.SetBorder(false)
	commandInput := tview.NewInputField().
		SetLabel("> ").
		SetLabelColor(logoColor).
		SetFieldTextColor(logoColor).
		SetFieldBackgroundColor(tcell.ColorDefault)
	commandInput.SetBorder(false)
	// Underline shown under the "> command" line when that line has focus
	commandUnderlineView := tview.NewTextView().SetDynamicColors(true)
	commandUnderlineView.SetBorder(false)
	outputView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetChangedFunc(func() { app.Draw() })
	outputView.SetBorder(false)

	updateDescriptionAndCommand := func() {
		desc := ""
		if stageIndex < len(stageDescriptions) {
			desc = stageDescriptions[stageIndex]
		}
		if stageIndex == 2 && !isLintAvailable() {
			desc += "  [yellow](not logged in — may fail; run 'atlas login')[-]"
		}
		descriptionView.SetText("[#98E0EA::b]" + desc + "[-]")
		commandInput.SetText(projectedCommand(stageIndex, getCurrentEnvName()))
	}

	bodyFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(descriptionView, 1, 0, false).
		AddItem(commandInput, 1, 0, true).
		AddItem(commandUnderlineView, 1, 0, false).
		AddItem(outputView, 0, 1, true)
	bodyFlex.SetBorder(true).SetTitle(" Output ").
		SetBorderColor(logoColor).SetTitleColor(logoColor)

	// Footer: key hints only (docker + env moved to top right), same blue as output border
	footerView := tview.NewTextView().SetDynamicColors(true).SetTextColor(logoColor)
	footerView.SetBorder(false)
	const footerKeysNormal = "  tab/shift+tab:stage • ↓/↑:scroll • enter:run • i:edit cmd • e:env • c:config • h:help • q:quit"
	const footerKeysEdit = "  [edit mode — Esc to exit, Enter to run]"
	updateFooter := func() {
		if editMode {
			footerView.SetText(footerKeysEdit)
		} else {
			footerView.SetText(footerKeysNormal)
		}
		updateTopRight()
	}

	// updateUI refreshes stage row and command underline based on editMode
	updateUI := func() {
		// Stage row always shows current stage highlighted (no underline needed since we use Tab now)
		stageRowView.SetText(buildStageRowText(stageIndex, false))
		if editMode {
			commandUnderlineView.SetText("[#98E0EA]" + strings.Repeat("─", 120) + "[-]")
		} else {
			commandUnderlineView.SetText("")
		}
		updateFooter()
	}

	// highlightStageOnly updates stage row text (preserving underline if stage has focus)
	highlightStageOnly := func(idx int) {
		stageRowView.SetText(buildStageRowText(idx, app.GetFocus() == stageRowView))
	}

	// highlightStage updates stage row and description/command in body
	highlightStage := func(idx int) {
		highlightStageOnly(idx)
		updateDescriptionAndCommand()
		outputView.SetText("")
	}
	highlightStage(0)
	updateFooter()

	// Check Docker availability (non-blocking)
	checkDocker := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "docker", "info")
		cmd.Stdout = nil
		cmd.Stderr = nil
		err := cmd.Run()
		statusMu.Lock()
		dockerOK = (err == nil)
		statusMu.Unlock()
		app.QueueUpdate(func() { updateFooter() })
	}
	go checkDocker()

	// Check Atlas Cloud login status (non-blocking)
	checkAtlasLogin := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "atlas", "whoami")
		cmd.Stdout = nil
		cmd.Stderr = nil
		err := cmd.Run()
		statusMu.Lock()
		atlasLoggedIn = (err == nil)
		statusMu.Unlock()
		app.QueueUpdate(func() {
			updateTopRight()
			highlightStageOnly(stageIndex) // Re-highlight to update Lint visibility
		})
	}
	go checkAtlasLogin()

	// .env watcher: keep env overlay in sync and refresh UI when .env changes
	go func() {
		loadEnv(envPath, envOverrides, &envMu)
		app.QueueUpdateDraw(func() {
			updateTopRight()
			updateDescriptionAndCommand()
			highlightStageOnly(stageIndex)
		})
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return
		}
		defer watcher.Close()
		if err := watcher.Add(workDir); err != nil {
			return
		}
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if (event.Op&(fsnotify.Write|fsnotify.Create) != 0) && filepath.Base(event.Name) == ".env" {
					loadEnv(envPath, envOverrides, &envMu)
					app.QueueUpdateDraw(func() {
						updateTopRight()
						updateDescriptionAndCommand()
						highlightStageOnly(stageIndex)
					})
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	// envForAtlas returns os.Environ() with .env overlay (so atlas subprocess sees ENVIRONMENT/APP_DB_URL from .env).
	envForAtlas := func() []string {
		envMu.Lock()
		overrides := make(map[string]string, len(envOverrides))
		for k, v := range envOverrides {
			overrides[k] = v
		}
		envMu.Unlock()
		base := make([]string, len(os.Environ()))
		copy(base, os.Environ())
		for k, v := range overrides {
			kv := k + "=" + v
			found := false
			for i, e := range base {
				if strings.HasPrefix(e, k+"=") {
					base[i] = kv
					found = true
					break
				}
			}
			if !found {
				base = append(base, kv)
			}
		}
		return base
	}
	runAtlas := func(args ...string) (stdout, stderr string, err error) {
		cmd := exec.Command("atlas", args...)
		cmd.Dir = workDir
		cmd.Env = envForAtlas()
		cmd.Stdin = nil // don't attach terminal stdin; child gets EOF so it never blocks on read
		var out, errOut strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		err = cmd.Run()
		return out.String(), errOut.String(), err
	}

	// Root layout: top (logo + docker/env) | strip (indented) | spacer | body | footer
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(topFlex, 6, 0, false).
		AddItem(stageStripRow, 1, 0, false).
		AddItem(spacerBelowStages, 1, 0, false).
		AddItem(bodyFlex, 0, 1, true).
		AddItem(footerView, 1, 0, false)
	// Floating overlay for Apply confirmation (drawn on top of root instead of replacing screen)
	var applyOverlay tview.Primitive
	rootWithOverlay := newOverlayRoot(root, &applyOverlay)
	// cmdLine returns the exact shell command for display (e.g. "atlas schema inspect --env local").
	cmdLine := func(args ...string) string { return "atlas " + strings.Join(args, " ") }

	// runCommandFromInput runs the command line from the input field (e.g. "atlas migrate status --env local").
	runCommandFromInput := func() {
		if running {
			return
		}
		text := strings.TrimSpace(commandInput.GetText())
		if text == "" {
			return
		}
		parts := strings.Fields(text)
		if len(parts) < 1 || parts[0] != "atlas" {
			outputView.SetText("Command must start with 'atlas' (e.g. atlas migrate status --env local)")
			outputView.ScrollToBeginning()
			return
		}
		args := parts[1:]
		running = true
		outputView.SetText("Running...")
		outputView.ScrollToBeginning()
		go func() {
			defer func() { running = false }()
			out, errOut, err := runAtlas(args...)
			app.QueueUpdate(func() {
				if err != nil {
					outputView.SetText(fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", err, errOut, out))
				} else {
					outputView.SetText(out + errOut)
				}
				outputView.ScrollToBeginning()
			})
		}()
	}

	runStage := func() {
		if running {
			return
		}
		running = true
		env := getCurrentEnvName()
		go func() {
			defer func() { running = false }()
			switch stageIndex {
			case 0: // Status - show applied vs pending
				out, errOut, err := runAtlas("migrate", "status", "--env", env)
				app.QueueUpdate(func() {
					if err != nil {
						outputView.SetText(fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", err, errOut, out))
						outputView.ScrollToBeginning()
						return
					}
					outputView.SetText(out + errOut)
					outputView.ScrollToBeginning()
				})
			case 1: // Diff - generate migration file
				out, errOut, err := runAtlas("migrate", "diff", "--env", env)
				app.QueueUpdate(func() {
					if err != nil {
						outputView.SetText(fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", err, errOut, out))
						outputView.ScrollToBeginning()
						return
					}
					outputView.SetText(out + errOut + "\n\n[gray]Tab to move to next stage.[-]")
					outputView.ScrollToBeginning()
				})
			case 2: // Lint (includes Hash)
				hashOut, hashErrOut, hashErr := runAtlas("migrate", "hash", "--env", env)
				lintCmdStr := cmdLine("migrate", "lint", "--env", env)
				lintOut, lintErrOut, lintErr := runAtlas("migrate", "lint", "--env", env)
				app.QueueUpdate(func() {
					if hashErr != nil {
						outputView.SetText(fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", hashErr, hashErrOut, hashOut))
						outputView.ScrollToBeginning()
						return
					}
					outputView.SetText(hashOut + hashErrOut + "\n\n> " + lintCmdStr + "\n\n" + lintOut + lintErrOut)
					if lintErr != nil {
						outputView.SetText(hashOut + hashErrOut + "\n\n> " + lintCmdStr + "\n\n" +
							fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", lintErr, lintErrOut, lintOut))
					}
					outputView.ScrollToBeginning()
				})
			case 3: // Preview (dry-run)
				cmdStr := cmdLine("migrate", "apply", "--env", env, "--dry-run")
				out, errOut, err := runAtlas("migrate", "apply", "--env", env, "--dry-run")
				app.QueueUpdate(func() {
					if err != nil {
						outputView.SetText(fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", err, errOut, out))
						outputView.ScrollToBeginning()
						return
					}
					previewText := out + errOut
					prefix := "> " + cmdStr + "\n\n"
					highlighted := highlightSQL(prefix + previewText)
					// Show in modal with scrollable TextView
					tv := tview.NewTextView().SetText(highlighted).SetScrollable(true).SetDynamicColors(false)
					tv.SetBorder(true).SetTitle(" Preview (dry-run) ").SetTitleAlign(tview.AlignLeft)
					previewFooter := tview.NewTextView().SetText(" Esc / q / Ctrl+C to close ").SetTextAlign(tview.AlignCenter)
					previewFooter.SetBorder(false)
					closePreview := func() {
						inOverlay = false
						app.SetRoot(rootWithOverlay, true).SetFocus(outputView)
						updateUI()
						// No auto-advance - user manually moves with arrow keys
					}
					flex := tview.NewFlex().SetDirection(tview.FlexRow).
						AddItem(tv, 0, 1, true).
						AddItem(previewFooter, 1, 0, false)
					captureClose := func(event *tcell.EventKey) *tcell.EventKey {
						switch event.Key() {
						case tcell.KeyEscape:
							closePreview()
							return nil
						case tcell.KeyCtrlC:
							closePreview()
							return nil
						}
						if event.Key() == tcell.KeyRune && (event.Rune() == 'q' || event.Rune() == 'Q') {
							closePreview()
							return nil
						}
						return event
					}
					flex.SetInputCapture(captureClose)
					tv.SetInputCapture(captureClose) // focus is on tv so capture there too
					inOverlay = true
					app.SetRoot(flex, true).SetFocus(tv)
				})
			case 4: // Apply
				out, errOut, err := runAtlas("migrate", "apply", "--env", env)
				app.QueueUpdate(func() {
					if err != nil {
						outputView.SetText(fmt.Sprintf("Error: %v\n\nStderr:\n%s\nStdout:\n%s", err, errOut, out))
						outputView.ScrollToBeginning()
						return
					}
					outputView.SetText("Apply completed successfully.\n\n" + out + errOut)
					outputView.ScrollToBeginning()
				})
			}
			// No auto-advance - user manually moves between stages with arrow keys
		}()
	}

	// Global key capture
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			// Exit edit mode if in it
			if editMode {
				editMode = false
				app.SetFocus(outputView)
				updateUI()
				return nil
			}
			// When inOverlay, let overlay handle Esc
			if inOverlay {
				return event
			}
			return nil // Do nothing on main screen (use 'q' to quit)
		case tcell.KeyTab:
			// Next stage
			if inOverlay || editMode {
				return event
			}
			if stageIndex < len(stages)-1 {
				stageIndex++
			} else {
				stageIndex = 0 // wrap around
			}
			highlightStage(stageIndex)
			return nil
		case tcell.KeyBacktab:
			// Previous stage (Shift+Tab)
			if inOverlay || editMode {
				return event
			}
			if stageIndex > 0 {
				stageIndex--
			} else {
				stageIndex = len(stages) - 1 // wrap around
			}
			highlightStage(stageIndex)
			return nil
		case tcell.KeyDown:
			// Scroll output down
			if inOverlay || editMode {
				return event
			}
			row, col := outputView.GetScrollOffset()
			outputView.ScrollTo(row+1, col)
			return nil
		case tcell.KeyUp:
			// Scroll output up
			if inOverlay || editMode {
				return event
			}
			row, col := outputView.GetScrollOffset()
			if row > 0 {
				outputView.ScrollTo(row-1, col)
			}
			return nil
		case tcell.KeyLeft, tcell.KeyRight:
			// In edit mode, let commandInput handle left/right
			if editMode {
				return event
			}
			// In overlay, let overlay handle
			if inOverlay {
				return event
			}
			return nil // consume on main screen
		case tcell.KeyEnter:
			if inOverlay {
				return event // let modal (e.g. help) handle Enter
			}
			if running {
				return nil
			}
			// If in edit mode, run the command and exit edit mode
			if editMode {
				editMode = false
				app.SetFocus(outputView)
				updateUI()
				runCommandFromInput()
				return nil
			}
			// From main screen: run current stage
			// For Apply stage, show confirmation (floating over the window)
			if stageIndex == 4 {
				closeApplyModal := func() {
					applyOverlay = nil
					inOverlay = false
					app.SetFocus(outputView)
					updateUI()
				}
				modal := tview.NewModal().
					SetText("Apply changes to database?").
					AddButtons([]string{"Apply", "Cancel"}).
					SetDoneFunc(func(buttonIndex int, buttonLabel string) {
						applyOverlay = nil
						inOverlay = false
						app.SetFocus(outputView)
						updateUI()
						if buttonLabel == "Apply" {
							outputView.SetText("Running...")
							outputView.ScrollToBeginning()
							go runStage()
						}
					})
				if getCurrentEnvName() == "prod" {
					modal.SetBorderColor(tcell.ColorRed)
				}
				modal.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
					switch event.Key() {
					case tcell.KeyEscape:
						closeApplyModal()
						return nil
					case tcell.KeyCtrlC:
						closeApplyModal()
						return nil
					case tcell.KeyLeft:
						return tcell.NewEventKey(tcell.KeyUp, 0, event.Modifiers())
					case tcell.KeyRight:
						return tcell.NewEventKey(tcell.KeyDown, 0, event.Modifiers())
					case tcell.KeyUp, tcell.KeyDown:
						return nil
					}
					if event.Key() == tcell.KeyRune && (event.Rune() == 'q' || event.Rune() == 'Q') {
						closeApplyModal()
						return nil
					}
					return event
				})
				applyOverlay = modal
				inOverlay = true
				app.SetFocus(modal)
				return nil
			}
			// Update UI on main thread (do NOT call app.Draw() here — it deadlocks). Event loop will redraw after we return.
			outputView.SetText("Running...")
			outputView.ScrollToBeginning()
			go runStage()
			return nil
		case tcell.KeyCtrlC:
			// Exit edit mode if in it
			if editMode {
				editMode = false
				app.SetFocus(outputView)
				updateUI()
				return nil
			}
			// When in overlay, let overlay handle (close); otherwise quit
			if !inOverlay {
				app.Stop()
				return nil
			}
			return event
		case tcell.KeyRune:
			// When in edit mode, let all characters pass through to commandInput
			if editMode {
				return event
			}
			switch event.Rune() {
			case 'q', 'Q':
				if inOverlay {
					return event // let config/preview/help close on q
				}
				app.Stop()
				return nil
			case 'i', 'I':
				// Enter edit mode (vim-like)
				if inOverlay {
					return event
				}
				editMode = true
				app.SetFocus(commandInput)
				updateUI()
				return nil
			case 'e', 'E':
				// Show current environment (from .env ENVIRONMENT)
				closeEnvModal := func() {
					applyOverlay = nil
					inOverlay = false
					app.SetFocus(stageRowView)
					updateUI()
				}
				currentEnv := getCurrentEnvName()
				modal := tview.NewModal().
					SetText(fmt.Sprintf("Current environment: %s\n\n(from .env ENVIRONMENT)\nEdit .env to change.", currentEnv)).
					AddButtons([]string{"OK"}).
					SetDoneFunc(func(buttonIndex int, buttonLabel string) {
						closeEnvModal()
					})
				modal.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
					switch event.Key() {
					case tcell.KeyEscape:
						closeEnvModal()
						return nil
					case tcell.KeyCtrlC:
						closeEnvModal()
						return nil
					case tcell.KeyLeft:
						return tcell.NewEventKey(tcell.KeyUp, 0, event.Modifiers())
					case tcell.KeyRight:
						return tcell.NewEventKey(tcell.KeyDown, 0, event.Modifiers())
					case tcell.KeyUp, tcell.KeyDown:
						return nil // consume so only ←/→ move between buttons
					}
					if event.Key() == tcell.KeyRune && (event.Rune() == 'q' || event.Rune() == 'Q') {
						closeEnvModal()
						return nil
					}
					return event
				})
				applyOverlay = modal
				inOverlay = true
				app.SetFocus(modal)
				return nil
			case 'c', 'C':
				// Config: in-app editor for atlas.hcl
				content, err := os.ReadFile(atlasHCL)
				if err != nil {
					// Don't use setBody here (uses QueueUpdate which can hang)
					outputView.SetText(fmt.Sprintf("Could not read atlas.hcl: %v", err))
					outputView.ScrollToBeginning()
					return nil
				}
				ta := tview.NewTextArea()
				ta.SetText(string(content), false)
				ta.SetOffset(0, 0)
				ta.SetBorder(true).SetTitle(" atlas.hcl ")
				ta.SetTitleAlign(tview.AlignLeft)
				saveAndClose := func() {
					newContent := ta.GetText()
					var msg string
					if err := os.WriteFile(atlasHCL, []byte(newContent), 0644); err != nil {
						msg = fmt.Sprintf("Could not write atlas.hcl: %v", err)
					} else {
						msg = "atlas.hcl saved."
						go checkDocker()
					}
					inOverlay = false
					app.SetRoot(rootWithOverlay, true).SetFocus(outputView)
					outputView.SetText(msg)
					outputView.ScrollToBeginning()
					updateUI()
				}
				closeEditorWithoutSave := func() {
					inOverlay = false
					app.SetRoot(rootWithOverlay, true).SetFocus(outputView)
					updateUI()
				}
				ta.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
					switch event.Key() {
					case tcell.KeyEscape:
						saveAndClose()
						return nil
					case tcell.KeyCtrlC:
						closeEditorWithoutSave()
						return nil
					}
					return event
				})
				editorFooter := tview.NewTextView().SetText(" Esc Save & exit   Ctrl+C Cancel ").SetTextAlign(tview.AlignCenter)
				editorFooter.SetBorder(false)
				editorFlex := tview.NewFlex().SetDirection(tview.FlexRow).
					AddItem(ta, 0, 1, true).
					AddItem(editorFooter, 1, 0, false)
				inOverlay = true
				app.SetRoot(editorFlex, true).SetFocus(ta)
				return nil
			case 'h', 'H':
				// Help dialog — fixed 80 columns (custom layout so width is respected)
				helpText := `Keys:
  Tab / Shift+Tab  — cycle through stages
  ↓/↑              — scroll output
  Enter            — run current stage command
  i                — edit command (vim-like: Esc to exit edit mode)
  e                — show current environment (from .env)
  c                — edit atlas.hcl config file
  h                — this help
  q                — quit

Stages: Status → Diff → Lint → Dry-Run → Apply
  Lint may fail if not logged in to Atlas Cloud (run 'atlas login')

Apply asks for confirmation (Apply or Cancel) before running.`
				closeHelp := func() {
					inOverlay = false
					app.SetRoot(rootWithOverlay, true).SetFocus(outputView)
					updateUI()
				}
				helpTV := tview.NewTextView().SetText(helpText).SetDynamicColors(false)
				helpOK := tview.NewButton("OK").SetSelectedFunc(closeHelp)
				helpBox := tview.NewFlex().SetDirection(tview.FlexRow).
					AddItem(helpTV, 0, 1, false).
					AddItem(helpOK, 1, 0, true)
				helpBox.SetBorder(true).SetTitle(" Help ")
				helpBox.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
					switch event.Key() {
					case tcell.KeyEscape:
						closeHelp()
						return nil
					case tcell.KeyEnter:
						closeHelp()
						return nil
					case tcell.KeyCtrlC:
						closeHelp()
						return nil
					}
					if event.Key() == tcell.KeyRune && (event.Rune() == 'q' || event.Rune() == 'Q') {
						closeHelp()
						return nil
					}
					return event
				})
				const helpWidth = 80
				helpWrap := tview.NewFlex().SetDirection(tview.FlexColumn).
					AddItem(nil, 0, 1, false).
					AddItem(helpBox, helpWidth, 0, true).
					AddItem(nil, 0, 1, false)
				inOverlay = true
				app.SetRoot(helpWrap, true).SetFocus(helpBox)
				return nil
			}
		}
		return event
	})

	app.SetRoot(rootWithOverlay, true).SetFocus(outputView)
	updateUI()
	// Run status automatically on start (must queue from a goroutine so main can enter Run() first; QueueUpdate blocks until the event loop runs the callback)
	go func() {
		app.QueueUpdate(func() {
			outputView.SetText("Running...")
			outputView.ScrollToBeginning()
			go runStage()
		})
	}()
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func hexToTCell(hex string) tcell.Color {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return tcell.ColorWhite
	}
	var r, g, b int
	_, _ = fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b)
	return tcell.NewRGBColor(int32(r), int32(g), int32(b))
}
