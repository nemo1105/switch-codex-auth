package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type candidate struct {
	Name           string
	Suffix         string
	Path           string
	Metadata       authMetadata
	Usage          string
	UsageRemaining usageRemainingMetrics
}

type usageRemainingMetrics struct {
	FiveHourRemaining    float64
	HasFiveHourRemaining bool
	SevenDayRemaining    float64
	HasSevenDayRemaining bool
}

type authMetadata struct {
	ModTime        time.Time
	AccessTime     time.Time
	HasAccessTime  bool
	LastRefresh    time.Time
	HasLastRefresh bool
}

var nowFunc = time.Now
var enrichCandidatesWithUsageFunc = enrichCandidatesWithUsage

func main() {
	if err := runCLI(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		exitErr(err)
	}
}

func runCLI(args []string, in io.Reader, out io.Writer) error {
	prog := filepath.Base(os.Args[0])
	if len(args) == 0 {
		return runInteractiveCommand(in, out, usageModeNone)
	}

	switch args[0] {
	case "-h", "--help":
		writeRootUsage(out, prog)
		return nil
	case "help":
		return runHelpCommand(args[1:], out, prog)
	case "list":
		return runListSubcommand(args[1:], out, prog)
	case "use":
		return runUseSubcommand(args[1:], out, prog)
	case "save":
		return runSaveSubcommand(args[1:], in, out, prog)
	case "login":
		return runLoginSubcommand(args[1:], in, out, prog)
	case "refresh":
		return runRefreshSubcommand(args[1:], out, prog)
	default:
		if err := legacyActionFlagError(args, prog); err != nil {
			return err
		}
		if strings.HasPrefix(args[0], "-") {
			usageMode, handled, err := parseRootInteractiveArgs(args, out, prog)
			if handled || err != nil {
				return err
			}
			return runInteractiveCommand(in, out, usageMode)
		}

		return fmt.Errorf("unknown command: %s (run `%s help` for usage)", args[0], prog)
	}
}

func runHelpCommand(args []string, out io.Writer, prog string) error {
	switch len(args) {
	case 0:
		writeRootUsage(out, prog)
		return nil
	case 1:
		switch args[0] {
		case "list":
			writeListUsage(out, prog)
			return nil
		case "use":
			writeUseUsage(out, prog)
			return nil
		case "save":
			writeSaveUsage(out, prog)
			return nil
		case "login":
			writeLoginUsage(out, prog)
			return nil
		case "refresh":
			writeRefreshUsage(out, prog)
			return nil
		default:
			return fmt.Errorf("unknown help topic: %s (run `%s help` for usage)", args[0], prog)
		}
	default:
		return fmt.Errorf("help accepts at most one command (usage: %s help [command])", prog)
	}
}

func runListSubcommand(args []string, out io.Writer, prog string) error {
	usageMode, handled, err := parseListSubcommandArgs(args, out, prog)
	if handled || err != nil {
		return err
	}

	return runListCommand(out, usageMode)
}

func runUseSubcommand(args []string, out io.Writer, prog string) error {
	selection, handled, err := parseUseSubcommandArgs(args, out, prog)
	if handled || err != nil {
		return err
	}

	return runUseCommand(selection, out)
}

func runSaveSubcommand(args []string, in io.Reader, out io.Writer, prog string) error {
	selection, force, handled, err := parseSaveSubcommandArgs(args, out, prog)
	if handled || err != nil {
		return err
	}

	return runSaveCommand(selection, force, in, out)
}

func runLoginSubcommand(args []string, in io.Reader, out io.Writer, prog string) error {
	selection, force, handled, err := parseLoginSubcommandArgs(args, out, prog)
	if handled || err != nil {
		return err
	}

	return runLoginCommand(selection, force, in, out)
}

func runRefreshSubcommand(args []string, out io.Writer, prog string) error {
	options, handled, err := parseRefreshSubcommandArgs(args, out, prog)
	if handled || err != nil {
		return err
	}

	return runRefreshCommand(out, options)
}

func runInteractiveCommand(in io.Reader, out io.Writer, usageMode usageMode) error {
	codexDir, candidates, current, currentMetadata, err := loadInteractiveState(usageMode)
	if err != nil {
		return err
	}

	return interactiveModeWithIO(codexDir, current, currentMetadata, candidates, in, out)
}

func runListCommand(out io.Writer, usageMode usageMode) error {
	codexDir, candidates, current, currentMetadata, err := loadInteractiveState(usageMode)
	if err != nil {
		return err
	}

	printStatus(out, codexDir, current, currentMetadata, candidates)
	return nil
}

func runUseCommand(selection string, out io.Writer) error {
	codexDir, candidates, current, err := loadSwitchState()
	if err != nil {
		return err
	}

	return switchToWithIO(codexDir, current, candidates, out, selection)
}

func runSaveCommand(selection string, force bool, in io.Reader, out io.Writer) error {
	codexDir, err := codexHome()
	if err != nil {
		return err
	}

	return saveCurrentAsWithIO(codexDir, selection, force, in, out, isInteractiveReader(in))
}

func runLoginCommand(selection string, force bool, in io.Reader, out io.Writer) error {
	codexDir, err := codexHome()
	if err != nil {
		return err
	}

	return loginAndSaveAlias(codexDir, selection, force, in, out, isInteractiveReader(in))
}

func runRefreshCommand(out io.Writer, options refreshOptions) error {
	codexDir, err := codexHome()
	if err != nil {
		return err
	}

	candidates, err := loadCandidates(codexDir)
	if err != nil {
		return err
	}

	return refreshAuthAliases(out, codexDir, candidates, options)
}

func parseRootInteractiveArgs(args []string, out io.Writer, prog string) (usageMode, bool, error) {
	flagSet := flag.NewFlagSet(prog, flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	usageValue := string(usageModeNone)
	flagSet.StringVar(&usageValue, "usage", string(usageModeNone), "usage fetch mode: none, api, or chat")

	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeRootUsage(out, prog)
			return usageModeNone, true, nil
		}
		return usageModeNone, false, fmt.Errorf("%v (run `%s help` for usage)", err, prog)
	}
	if flagSet.NArg() != 0 {
		return usageModeNone, false, fmt.Errorf("interactive mode does not accept arguments (usage: %s [--usage none|api|chat])", prog)
	}

	mode, err := parseUsageMode(usageValue)
	if err != nil {
		return usageModeNone, false, fmt.Errorf("%v (run `%s help` for usage)", err, prog)
	}
	return mode, false, nil
}

func parseListSubcommandArgs(args []string, out io.Writer, prog string) (usageMode, bool, error) {
	flagSet := flag.NewFlagSet("list", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	usageValue := string(usageModeNone)
	flagSet.StringVar(&usageValue, "usage", string(usageModeNone), "usage fetch mode: none, api, or chat")

	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeListUsage(out, prog)
			return usageModeNone, true, nil
		}
		return usageModeNone, false, fmt.Errorf("%v (run `%s help list` for usage)", err, prog)
	}
	if flagSet.NArg() != 0 {
		return usageModeNone, false, fmt.Errorf("list does not accept arguments (usage: %s list [--usage none|api|chat])", prog)
	}

	mode, err := parseUsageMode(usageValue)
	if err != nil {
		return usageModeNone, false, fmt.Errorf("%v (run `%s help list` for usage)", err, prog)
	}
	return mode, false, nil
}

func parseUseSubcommandArgs(args []string, out io.Writer, prog string) (string, bool, error) {
	flagSet := flag.NewFlagSet("use", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeUseUsage(out, prog)
			return "", true, nil
		}
		return "", false, fmt.Errorf("%v (run `%s help use` for usage)", err, prog)
	}

	switch flagSet.NArg() {
	case 1:
		return flagSet.Arg(0), false, nil
	case 0:
		return "", false, fmt.Errorf("use requires <suffix-or-index> (usage: %s use <suffix-or-index>)", prog)
	default:
		return "", false, fmt.Errorf("use accepts exactly one <suffix-or-index> (usage: %s use <suffix-or-index>)", prog)
	}
}

func parseSaveSubcommandArgs(args []string, out io.Writer, prog string) (string, bool, bool, error) {
	flagSet := flag.NewFlagSet("save", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	var force bool
	flagSet.BoolVar(&force, "f", false, "overwrite an existing auth alias")
	flagSet.BoolVar(&force, "force", false, "overwrite an existing auth alias")

	if err := flagSet.Parse(normalizeSaveArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeSaveUsage(out, prog)
			return "", false, true, nil
		}
		return "", false, false, fmt.Errorf("%v (run `%s help save` for usage)", err, prog)
	}

	switch flagSet.NArg() {
	case 1:
		return flagSet.Arg(0), force, false, nil
	case 0:
		return "", false, false, fmt.Errorf("save requires <suffix> (usage: %s save <suffix> [-f|--force])", prog)
	default:
		return "", false, false, fmt.Errorf("save accepts exactly one <suffix> (usage: %s save <suffix> [-f|--force])", prog)
	}
}

func parseLoginSubcommandArgs(args []string, out io.Writer, prog string) (string, bool, bool, error) {
	flagSet := flag.NewFlagSet("login", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	var force bool
	flagSet.BoolVar(&force, "f", false, "overwrite an existing auth alias")
	flagSet.BoolVar(&force, "force", false, "overwrite an existing auth alias")

	if err := flagSet.Parse(normalizeSaveArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeLoginUsage(out, prog)
			return "", false, true, nil
		}
		return "", false, false, fmt.Errorf("%v (run `%s help login` for usage)", err, prog)
	}

	switch flagSet.NArg() {
	case 0:
		return "", force, false, nil
	case 1:
		return flagSet.Arg(0), force, false, nil
	default:
		return "", false, false, fmt.Errorf("login accepts at most one <suffix> (usage: %s login [suffix] [-f|--force])", prog)
	}
}

func parseRefreshSubcommandArgs(args []string, out io.Writer, prog string) (refreshOptions, bool, error) {
	flagSet := flag.NewFlagSet("refresh", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	options := defaultRefreshOptions()
	flagSet.IntVar(&options.MinAgeDays, "days", defaultRefreshMinAgeDays, "minimum age in days since last_refresh")
	flagSet.BoolVar(&options.Force, "f", false, "refresh all refreshable auth aliases without last_refresh checks")
	flagSet.BoolVar(&options.Force, "force", false, "refresh all refreshable auth aliases without last_refresh checks")

	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeRefreshUsage(out, prog)
			return refreshOptions{}, true, nil
		}
		return refreshOptions{}, false, fmt.Errorf("%v (run `%s help refresh` for usage)", err, prog)
	}
	if flagSet.NArg() != 0 {
		return refreshOptions{}, false, fmt.Errorf("refresh does not accept arguments (usage: %s refresh [-f|--force] [--days N])", prog)
	}
	if options.MinAgeDays < 0 {
		return refreshOptions{}, false, fmt.Errorf("refresh days must be >= 0 (usage: %s refresh [-f|--force] [--days N])", prog)
	}

	return options, false, nil
}

func normalizeSaveArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))

	for _, arg := range args {
		switch arg {
		case "-f", "--force", "-h", "--help":
			flags = append(flags, arg)
		default:
			positionals = append(positionals, arg)
		}
	}

	return append(flags, positionals...)
}

func legacyActionFlagError(args []string, prog string) error {
	if len(args) == 0 {
		return nil
	}

	var replacement []string
	switch args[0] {
	case "--list":
		replacement = append([]string{"list"}, args[1:]...)
	case "--use":
		replacement = append([]string{"use"}, args[1:]...)
	case "--save":
		replacement = append([]string{"save"}, args[1:]...)
	case "--login":
		replacement = append([]string{"login"}, args[1:]...)
	case "--refresh":
		replacement = append([]string{"refresh"}, args[1:]...)
	default:
		return nil
	}

	return fmt.Errorf("legacy action flags are no longer supported: use `%s %s`", prog, strings.Join(replacement, " "))
}

func writeRootUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s [--usage none|api|chat]\n", prog)
	fmt.Fprintf(w, "  %s list [--usage none|api|chat]\n", prog)
	fmt.Fprintf(w, "  %s use <suffix-or-index>\n", prog)
	fmt.Fprintf(w, "  %s save <suffix> [-f|--force]\n", prog)
	fmt.Fprintf(w, "  %s login [suffix] [-f|--force]\n", prog)
	fmt.Fprintf(w, "  %s refresh [-f|--force] [--days N]\n", prog)
	fmt.Fprintf(w, "  %s help [command]\n", prog)
	fmt.Fprintf(w, "\nCommands:\n")
	fmt.Fprintf(w, "  list     Show the current auth and auth.json.* files, optionally with usage summaries\n")
	fmt.Fprintf(w, "  use      Switch to auth.json.<suffix> or a menu index\n")
	fmt.Fprintf(w, "  save     Copy the current auth.json to auth.json.<suffix>\n")
	fmt.Fprintf(w, "  login    Sign in with Codex OAuth and save the new auth as auth.json.<suffix>\n")
	fmt.Fprintf(w, "  refresh  Refresh auth.json.* files whose last_refresh is at least N days old unless forced (default 7)\n")
	fmt.Fprintf(w, "\nOptions:\n")
	fmt.Fprintf(w, "  --usage  Usage fetch mode for list and interactive mode: none, api, or chat (default none)\n")
	fmt.Fprintf(w, "\nEnvironment:\n")
	fmt.Fprintf(w, "  CODEX_HOME  Override the auth directory. Defaults to %s\n", defaultCodexHomeHint())
}

func writeListUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s list [--usage none|api|chat]\n", prog)
	fmt.Fprintf(w, "\nOptions:\n")
	fmt.Fprintf(w, "  --usage  Usage fetch mode: none, api, or chat (default none)\n")
}

func writeUseUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s use <suffix-or-index>\n", prog)
}

func writeSaveUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s save <suffix> [-f|--force]\n", prog)
}

func writeLoginUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s login [suffix] [-f|--force]\n", prog)
}

func writeRefreshUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s refresh [-f|--force] [--days N]\n", prog)
}

func loadInteractiveState(usageMode usageMode) (string, []candidate, string, *authMetadata, error) {
	codexDir, err := codexHome()
	if err != nil {
		return "", nil, "", nil, err
	}

	candidates, err := loadCandidates(codexDir)
	if err != nil {
		return "", nil, "", nil, err
	}

	currentMetadata, err := loadCurrentAuthMetadata(codexDir)
	if err != nil {
		return "", nil, "", nil, err
	}

	current, err := currentSuffix(codexDir, candidates)
	if err != nil {
		return "", nil, "", nil, err
	}

	if usageMode != usageModeNone {
		candidates = enrichCandidatesWithUsageFunc(candidates, usageMode)
	}

	return codexDir, candidates, current, currentMetadata, nil
}

func loadSwitchState() (string, []candidate, string, error) {
	codexDir, err := codexHome()
	if err != nil {
		return "", nil, "", err
	}

	candidates, err := loadCandidates(codexDir)
	if err != nil {
		return "", nil, "", err
	}

	current, err := currentSuffix(codexDir, candidates)
	if err != nil {
		return "", nil, "", err
	}

	return codexDir, candidates, current, nil
}

func isInteractiveReader(in io.Reader) bool {
	file, ok := in.(*os.File)
	if !ok {
		return false
	}

	return isInteractiveInput(file)
}

func codexHome() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return filepath.Clean(v), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	return filepath.Join(home, ".codex"), nil
}

func defaultCodexHomeHint() string {
	if runtime.GOOS == "windows" {
		return `%USERPROFILE%\.codex`
	}
	return `$HOME/.codex`
}

func loadCandidates(codexDir string) ([]candidate, error) {
	entries, err := os.ReadDir(codexDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("auth directory does not exist: %s", codexDir)
		}
		return nil, fmt.Errorf("read auth directory %s: %w", codexDir, err)
	}

	candidates := make([]candidate, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if !strings.HasPrefix(name, "auth.json.") {
			continue
		}

		candidates = append(candidates, candidate{
			Name:   name,
			Suffix: strings.TrimPrefix(name, "auth.json."),
			Path:   filepath.Join(codexDir, name),
		})
	}

	for i := range candidates {
		info, err := os.Stat(candidates[i].Path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", candidates[i].Path, err)
		}

		candidates[i].Metadata = loadAuthMetadata(candidates[i].Path, info)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Suffix < candidates[j].Suffix
	})

	return candidates, nil
}

func loadCurrentAuthMetadata(codexDir string) (*authMetadata, error) {
	activePath := filepath.Join(codexDir, "auth.json")
	info, err := os.Stat(activePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", activePath, err)
	}

	metadata := loadAuthMetadata(activePath, info)
	return &metadata, nil
}

func loadAuthMetadata(path string, info os.FileInfo) authMetadata {
	metadata := authMetadata{
		ModTime: info.ModTime(),
	}

	if accessTime, ok := fileAccessTime(info); ok {
		metadata.AccessTime = accessTime
		metadata.HasAccessTime = true
	}

	if lastRefresh, ok := readLastRefresh(path); ok {
		metadata.LastRefresh = lastRefresh
		metadata.HasLastRefresh = true
	}

	return metadata
}

func readLastRefresh(path string) (time.Time, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}

	var payload struct {
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return time.Time{}, false
	}

	value := strings.TrimSpace(payload.LastRefresh)
	if value == "" {
		return time.Time{}, false
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}

	return parsed, true
}

func currentSuffix(codexDir string, candidates []candidate) (string, error) {
	activePath := filepath.Join(codexDir, "auth.json")
	if _, err := os.Stat(activePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "none", nil
		}
		return "", fmt.Errorf("stat %s: %w", activePath, err)
	}

	activeEmail, ok, err := extractComparableEmailFromAuthJSON(activePath)
	if err != nil {
		return "", err
	}
	if !ok {
		return "custom/unmatched", nil
	}

	for _, candidate := range candidates {
		candidateEmail, ok, err := extractComparableEmailFromAuthJSON(candidate.Path)
		if err != nil {
			return "", err
		}
		if ok && candidateEmail == activeEmail {
			return candidate.Suffix, nil
		}
	}

	return "custom/unmatched", nil
}

func printStatus(w io.Writer, codexDir, current string, currentMetadata *authMetadata, candidates []candidate) {
	fmt.Fprintf(w, "Auth dir: %s\n", codexDir)

	if current == "custom/unmatched" && currentMetadata != nil {
		fmt.Fprintf(
			w,
			"Current auth details: mtime=%s, atime=%s, last_refresh=%s\n",
			formatDisplayTime(currentMetadata.ModTime, true),
			formatDisplayTime(currentMetadata.AccessTime, currentMetadata.HasAccessTime),
			formatDisplayTime(currentMetadata.LastRefresh, currentMetadata.HasLastRefresh),
		)
	}

	fmt.Fprintf(w, "Available auth files (%d):\n", len(candidates))
	if len(candidates) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}

	type candidateRow struct {
		marker      string
		index       string
		suffix      string
		lastRefresh string
		usage       string
	}

	rows := make([]candidateRow, 0, len(candidates))
	indexWidth := len("#")
	suffixWidth := len("Suffix")
	lastRefreshWidth := len("Last refresh")
	usageWidth := len("Usage")

	for i, candidate := range candidates {
		marker := " "
		if candidate.Suffix == current {
			marker = "*"
		}
		index := strconv.Itoa(i + 1)

		row := candidateRow{
			marker:      marker,
			index:       index,
			suffix:      candidate.Suffix,
			lastRefresh: formatDisplayTime(candidate.Metadata.LastRefresh, candidate.Metadata.HasLastRefresh),
			usage:       formatUsageDisplay(candidate.Usage),
		}
		rows = append(rows, row)

		if len(row.index) > indexWidth {
			indexWidth = len(row.index)
		}
		if len(row.suffix) > suffixWidth {
			suffixWidth = len(row.suffix)
		}
		if len(row.lastRefresh) > lastRefreshWidth {
			lastRefreshWidth = len(row.lastRefresh)
		}
		if len(row.usage) > usageWidth {
			usageWidth = len(row.usage)
		}
	}

	fmt.Fprintf(
		w,
		" %1s %-*s %-*s %-*s %-*s\n",
		" ",
		indexWidth,
		"#",
		suffixWidth,
		"Suffix",
		lastRefreshWidth,
		"Last refresh",
		usageWidth,
		"Usage",
	)

	for _, row := range rows {
		fmt.Fprintf(
			w,
			" %1s %-*s %-*s %-*s %-*s\n",
			row.marker,
			indexWidth,
			row.index,
			suffixWidth,
			row.suffix,
			lastRefreshWidth,
			row.lastRefresh,
			usageWidth,
			row.usage,
		)
	}

	fmt.Fprintln(w, "Hint:")
	fmt.Fprintln(w, "  * marks the current alias")
	fmt.Fprintln(w, "  Last refresh is a local file signal")
	fmt.Fprintln(w, "  Usage is shown only with --usage api|chat and may show n/a or a concise error per alias")
}

func formatUsageDisplay(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func formatDisplayTime(value time.Time, ok bool) string {
	if !ok {
		return "-"
	}
	return formatRelativeTime(value, nowFunc())
}

func formatRelativeTime(value, now time.Time) string {
	delta := now.Sub(value)
	if delta < 0 {
		return "in " + formatRelativeAmount(-delta)
	}
	return formatRelativeAmount(delta) + " ago"
}

func formatRelativeAmount(delta time.Duration) string {
	switch {
	case delta < time.Minute:
		return "<1m"
	case delta < time.Hour:
		return fmt.Sprintf("%dm", int(delta/time.Minute))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh", int(delta/time.Hour))
	case delta < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(delta/(24*time.Hour)))
	case delta < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(delta/(30*24*time.Hour)))
	default:
		return fmt.Sprintf("%dy", int(delta/(365*24*time.Hour)))
	}
}

func interactiveMode(codexDir, current string, currentMetadata *authMetadata, candidates []candidate) error {
	return interactiveModeWithIO(codexDir, current, currentMetadata, candidates, os.Stdin, os.Stdout)
}

func interactiveModeWithIO(
	codexDir,
	current string,
	currentMetadata *authMetadata,
	candidates []candidate,
	in io.Reader,
	out io.Writer,
) error {
	printStatus(out, codexDir, current, currentMetadata, candidates)

	if len(candidates) == 0 {
		return fmt.Errorf("no auth.json.* files found in %s", codexDir)
	}

	refreshTask, err := startInteractiveRefresh(codexDir, candidates)
	if err != nil {
		return err
	}
	if refreshTask != nil {
		fmt.Fprintf(out, "Refreshing stale auth aliases in background: %s\n", strings.Join(refreshTask.aliasNames, ", "))
	}

	reader := bufio.NewReader(in)
	defaultCandidate, hasDefault := defaultInteractiveCandidate(candidates)

	for {
		if hasDefault {
			fmt.Fprintf(out, "Choose auth file by number or suffix [default: %s]: ", defaultCandidate.Suffix)
		} else {
			fmt.Fprint(out, "Choose auth file by number or suffix: ")
		}

		selection, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			if refreshErr := finishInteractiveRefresh(out, refreshTask, candidate{}, false); refreshErr != nil {
				return refreshErr
			}
			return fmt.Errorf("read selection: %w", err)
		}

		selection = strings.TrimSpace(selection)
		if selection == "" {
			if hasDefault {
				selection = defaultCandidate.Suffix
			} else if errors.Is(err, io.EOF) {
				if refreshErr := finishInteractiveRefresh(out, refreshTask, candidate{}, false); refreshErr != nil {
					return refreshErr
				}
				return fmt.Errorf("read selection: %w", err)
			} else {
				fmt.Fprintln(out, "Invalid selection: no default is available. Enter a number or suffix.")
				continue
			}
		}

		target, switchErr := switchToCandidateWithIO(codexDir, current, candidates, out, selection)
		refreshErr := finishInteractiveRefresh(out, refreshTask, target, switchErr == nil)
		if switchErr != nil {
			return switchErr
		}
		return refreshErr
	}
}

func switchTo(codexDir, current string, candidates []candidate, selection string) error {
	return switchToWithIO(codexDir, current, candidates, os.Stdout, selection)
}

func switchToWithIO(codexDir, current string, candidates []candidate, out io.Writer, selection string) error {
	_, err := switchToCandidateWithIO(codexDir, current, candidates, out, selection)
	return err
}

func switchToCandidateWithIO(codexDir, current string, candidates []candidate, out io.Writer, selection string) (candidate, error) {
	if len(candidates) == 0 {
		return candidate{}, fmt.Errorf("no auth.json.* files found in %s", codexDir)
	}

	target, err := resolveTarget(candidates, selection)
	if err != nil {
		return candidate{}, err
	}

	if current == target.Suffix {
		fmt.Fprintf(out, "Already using: %s\n", target.Suffix)
		return target, nil
	}

	if err := replaceAuthFile(codexDir, target.Path); err != nil {
		return candidate{}, err
	}

	fmt.Fprintf(out, "Switched auth to: %s\n", target.Suffix)
	return target, nil
}

type interactiveRefreshTask struct {
	codexDir   string
	candidates []candidate
	aliasNames []string
	done       <-chan refreshReport
}

func startInteractiveRefresh(codexDir string, candidates []candidate) (*interactiveRefreshTask, error) {
	plan, err := buildRefreshPlan(codexDir, candidates, defaultRefreshOptions())
	if err != nil {
		return nil, err
	}
	if len(plan.refreshTokens) == 0 {
		return nil, nil
	}

	done := make(chan refreshReport, 1)
	task := &interactiveRefreshTask{
		codexDir:   plan.codexDir,
		candidates: append([]candidate(nil), plan.candidates...),
		aliasNames: refreshPlanAliasNames(plan),
		done:       done,
	}

	go func(plan refreshPlan) {
		executeRefreshPlan(&plan)
		done <- completeRefreshReport(plan)
	}(plan)

	return task, nil
}

func finishInteractiveRefresh(out io.Writer, task *interactiveRefreshTask, selected candidate, hasSelected bool) error {
	if task == nil {
		return nil
	}

	var report refreshReport
	select {
	case report = <-task.done:
	default:
		fmt.Fprintln(out, "Auth refresh is still running; waiting for it to finish...")
		report = <-task.done
	}

	fmt.Fprintln(out, "Auth refresh results:")
	renderRefreshReport(out, task.candidates, report)

	if hasSelected && refreshReportHasRefreshedCandidate(task.candidates, report, selected) {
		if err := replaceAuthFile(task.codexDir, selected.Path); err != nil {
			return fmt.Errorf("sync refreshed active auth: %w", err)
		}
		fmt.Fprintf(out, "Updated active auth from refreshed profile: %s\n", selected.Suffix)
	}

	return nil
}

func refreshPlanAliasNames(plan refreshPlan) []string {
	refreshIndexes := make(map[int]bool)
	for _, token := range plan.refreshTokens {
		for _, index := range plan.groups[token] {
			refreshIndexes[index] = true
		}
	}

	names := make([]string, 0, len(refreshIndexes))
	for i, candidate := range plan.candidates {
		if refreshIndexes[i] {
			names = append(names, candidate.Suffix)
		}
	}
	return names
}

func refreshReportHasRefreshedCandidate(candidates []candidate, report refreshReport, selected candidate) bool {
	for i, candidate := range candidates {
		if candidate.Path != selected.Path && candidate.Suffix != selected.Suffix {
			continue
		}
		if i >= len(report.Results) {
			return false
		}
		return report.Results[i].Status == refreshResultRefreshed
	}
	return false
}

func resolveTarget(candidates []candidate, selection string) (candidate, error) {
	selection = normalizeSelection(selection)

	for _, candidate := range candidates {
		if candidate.Suffix == selection {
			return candidate, nil
		}
	}

	if index, err := strconv.Atoi(selection); err == nil {
		if index >= 1 && index <= len(candidates) {
			return candidates[index-1], nil
		}
	}

	return candidate{}, fmt.Errorf("unknown auth target: %s", selection)
}

func defaultInteractiveCandidate(candidates []candidate) (candidate, bool) {
	var best candidate
	hasBest := false

	for _, candidate := range candidates {
		usage := candidate.UsageRemaining
		if !usage.HasFiveHourRemaining || usage.FiveHourRemaining <= 0 {
			continue
		}

		if !hasBest ||
			usage.FiveHourRemaining > best.UsageRemaining.FiveHourRemaining ||
			(usage.FiveHourRemaining == best.UsageRemaining.FiveHourRemaining &&
				sevenDayRemainingForDefault(usage) > sevenDayRemainingForDefault(best.UsageRemaining)) {
			best = candidate
			hasBest = true
		}
	}

	return best, hasBest
}

func sevenDayRemainingForDefault(usage usageRemainingMetrics) float64 {
	if !usage.HasSevenDayRemaining {
		return 0
	}
	return usage.SevenDayRemaining
}

func normalizeSelection(selection string) string {
	selection = strings.TrimSpace(selection)
	if strings.HasPrefix(selection, "auth.json.") {
		return strings.TrimPrefix(selection, "auth.json.")
	}
	return selection
}

func saveCurrentAs(codexDir, selection string, force bool) error {
	return saveCurrentAsWithIO(codexDir, selection, force, os.Stdin, os.Stdout, isInteractiveInput(os.Stdin))
}

func saveCurrentAsWithIO(codexDir, selection string, force bool, in io.Reader, out io.Writer, interactive bool) error {
	activePath := filepath.Join(codexDir, "auth.json")
	if _, err := os.Stat(activePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("active auth file does not exist: %s", activePath)
		}
		return fmt.Errorf("stat %s: %w", activePath, err)
	}

	suffix, err := normalizeSuffix(selection)
	if err != nil {
		return err
	}

	_, err = saveSourceAsAliasWithIO(codexDir, activePath, suffix, force, in, out, interactive, "Saved current auth as")
	return err
}

func saveSourceAsAliasWithIO(
	codexDir string,
	sourcePath string,
	suffix string,
	force bool,
	in io.Reader,
	out io.Writer,
	interactive bool,
	successPrefix string,
) (string, error) {
	var reader *bufio.Reader
	if interactive {
		reader = bufio.NewReader(in)
	}
	return saveSourceAsAliasWithReader(codexDir, sourcePath, suffix, force, reader, out, successPrefix)
}

func saveSourceAsAliasWithReader(
	codexDir string,
	sourcePath string,
	suffix string,
	force bool,
	reader *bufio.Reader,
	out io.Writer,
	successPrefix string,
) (string, error) {
	for {
		targetPath := filepath.Join(codexDir, "auth.json."+suffix)
		if _, err := os.Stat(targetPath); err == nil {
			switch {
			case force:
				if err := copyFileToTarget(sourcePath, targetPath, true); err != nil {
					return "", err
				}
				fmt.Fprintf(out, "Overwrote auth alias: %s\n", suffix)
				return suffix, nil
			case reader == nil:
				return "", fmt.Errorf("auth alias already exists: %s (use --force to overwrite or choose a different alias)", filepath.Base(targetPath))
			default:
				nextSuffix, overwrite, err := promptForSaveConflict(reader, out, targetPath)
				if err != nil {
					return "", err
				}
				if overwrite {
					if err := copyFileToTarget(sourcePath, targetPath, true); err != nil {
						return "", err
					}
					fmt.Fprintf(out, "Overwrote auth alias: %s\n", suffix)
					return suffix, nil
				}
				if nextSuffix == "" {
					continue
				}
				suffix = nextSuffix
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", targetPath, err)
		}

		if err := copyFileToTarget(sourcePath, targetPath, false); err != nil {
			return "", err
		}

		fmt.Fprintf(out, "%s: %s\n", successPrefix, suffix)
		return suffix, nil
	}
}

func isInteractiveInput(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func promptForSaveConflict(reader *bufio.Reader, out io.Writer, targetPath string) (string, bool, error) {
	fmt.Fprintf(out, "Auth alias already exists: %s\n", filepath.Base(targetPath))
	fmt.Fprint(out, "Press Enter to overwrite, or type a different alias to save as: ")

	selection, err := readPromptLine(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", false, fmt.Errorf("save cancelled before resolving existing alias: %s", filepath.Base(targetPath))
		}
		return "", false, fmt.Errorf("read save selection: %w", err)
	}

	selection = strings.TrimSpace(selection)
	if selection == "" {
		return "", true, nil
	}

	suffix, err := normalizeSuffix(selection)
	if err != nil {
		fmt.Fprintf(out, "Invalid alias: %v\n", err)
		return "", false, nil
	}

	return suffix, false, nil
}

func readPromptLine(reader *bufio.Reader) (string, error) {
	selection, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && selection != "" {
			return selection, nil
		}
		return "", err
	}
	return selection, nil
}

func normalizeSuffix(selection string) (string, error) {
	suffix := normalizeSelection(selection)
	if suffix == "" {
		return "", fmt.Errorf("suffix cannot be empty")
	}
	if suffix == "." || suffix == ".." {
		return "", fmt.Errorf("invalid suffix: %s", suffix)
	}
	if strings.Contains(suffix, "/") || strings.Contains(suffix, "\\") {
		return "", fmt.Errorf("suffix cannot contain path separators: %s", suffix)
	}
	return suffix, nil
}

func replaceAuthFile(codexDir, sourcePath string) error {
	return copyFileToTarget(sourcePath, filepath.Join(codexDir, "auth.json"), true)
}

func copyFileToTarget(sourcePath, targetPath string, overwrite bool) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer source.Close()

	tmpDir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(tmpDir, "auth.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp auth file: %w", err)
	}

	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, source); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp auth file: %w", err)
	}

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp auth file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp auth file: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		if !overwrite {
			return fmt.Errorf("move temp auth file to %s: %w", targetPath, err)
		}
		if removeErr := os.Remove(targetPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("replace %s: rename failed (%v), remove failed (%w)", targetPath, err, removeErr)
		}
		if renameErr := os.Rename(tmpPath, targetPath); renameErr != nil {
			return fmt.Errorf("replace %s after remove: %w", targetPath, renameErr)
		}
	}

	cleanup = false
	return nil
}

func exitErr(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func exitf(format string, args ...any) {
	exitErr(fmt.Errorf(format, args...))
}
