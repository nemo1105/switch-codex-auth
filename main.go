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
	Name     string
	Suffix   string
	Path     string
	Metadata authMetadata
	Usage    string
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
		return runInteractiveCommand(in, out)
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
	case "refresh":
		return runRefreshSubcommand(args[1:], out, prog)
	default:
		if err := legacyActionFlagError(args, prog); err != nil {
			return err
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
	handled, err := parseNoArgSubcommand("list", args, out, prog, writeListUsage)
	if handled || err != nil {
		return err
	}

	return runListCommand(out)
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

func runRefreshSubcommand(args []string, out io.Writer, prog string) error {
	days, handled, err := parseRefreshSubcommandArgs(args, out, prog)
	if handled || err != nil {
		return err
	}

	return runRefreshCommand(out, days)
}

func runInteractiveCommand(in io.Reader, out io.Writer) error {
	codexDir, candidates, current, currentMetadata, err := loadInteractiveState()
	if err != nil {
		return err
	}

	return interactiveModeWithIO(codexDir, current, currentMetadata, candidates, in, out)
}

func runListCommand(out io.Writer) error {
	codexDir, candidates, current, currentMetadata, err := loadInteractiveState()
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

func runRefreshCommand(out io.Writer, days int) error {
	codexDir, err := codexHome()
	if err != nil {
		return err
	}

	candidates, err := loadCandidates(codexDir)
	if err != nil {
		return err
	}

	return refreshAuthAliases(out, codexDir, candidates, days)
}

func parseNoArgSubcommand(
	name string,
	args []string,
	out io.Writer,
	prog string,
	usage func(io.Writer, string),
) (bool, error) {
	flagSet := flag.NewFlagSet(name, flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(out, prog)
			return true, nil
		}
		return false, fmt.Errorf("%v (run `%s help %s` for usage)", err, prog, name)
	}
	if flagSet.NArg() != 0 {
		return false, fmt.Errorf("%s does not accept arguments (usage: %s %s)", name, prog, name)
	}

	return false, nil
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

func parseRefreshSubcommandArgs(args []string, out io.Writer, prog string) (int, bool, error) {
	flagSet := flag.NewFlagSet("refresh", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)

	days := defaultRefreshMinAgeDays
	flagSet.IntVar(&days, "days", defaultRefreshMinAgeDays, "minimum age in days since last_refresh")

	if err := flagSet.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeRefreshUsage(out, prog)
			return 0, true, nil
		}
		return 0, false, fmt.Errorf("%v (run `%s help refresh` for usage)", err, prog)
	}
	if flagSet.NArg() != 0 {
		return 0, false, fmt.Errorf("refresh does not accept arguments (usage: %s refresh [--days N])", prog)
	}
	if days < 0 {
		return 0, false, fmt.Errorf("refresh days must be >= 0 (usage: %s refresh [--days N])", prog)
	}

	return days, false, nil
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
	case "--refresh":
		replacement = append([]string{"refresh"}, args[1:]...)
	default:
		return nil
	}

	return fmt.Errorf("legacy action flags are no longer supported: use `%s %s`", prog, strings.Join(replacement, " "))
}

func writeRootUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s\n", prog)
	fmt.Fprintf(w, "  %s list\n", prog)
	fmt.Fprintf(w, "  %s use <suffix-or-index>\n", prog)
	fmt.Fprintf(w, "  %s save <suffix> [-f|--force]\n", prog)
	fmt.Fprintf(w, "  %s refresh [--days N]\n", prog)
	fmt.Fprintf(w, "  %s help [command]\n", prog)
	fmt.Fprintf(w, "\nCommands:\n")
	fmt.Fprintf(w, "  list     Show the current auth, auth.json.* files, and live usage summaries\n")
	fmt.Fprintf(w, "  use      Switch to auth.json.<suffix> or a menu index\n")
	fmt.Fprintf(w, "  save     Copy the current auth.json to auth.json.<suffix>\n")
	fmt.Fprintf(w, "  refresh  Refresh auth.json.* files whose last_refresh is at least N days old (default 7)\n")
	fmt.Fprintf(w, "\nEnvironment:\n")
	fmt.Fprintf(w, "  CODEX_HOME  Override the auth directory. Defaults to %s\n", defaultCodexHomeHint())
}

func writeListUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s list\n", prog)
}

func writeUseUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s use <suffix-or-index>\n", prog)
}

func writeSaveUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s save <suffix> [-f|--force]\n", prog)
}

func writeRefreshUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s refresh [--days N]\n", prog)
}

func loadInteractiveState() (string, []candidate, string, *authMetadata, error) {
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

	candidates = enrichCandidatesWithUsageFunc(candidates)

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
	fmt.Fprintln(w, "  Usage is live account data and may show n/a or a concise error per alias")
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

	fmt.Fprint(out, "Choose auth file by number or suffix (Enter to cancel): ")
	reader := bufio.NewReader(in)
	selection, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read selection: %w", err)
	}

	selection = strings.TrimSpace(selection)
	if selection == "" {
		fmt.Fprintln(out, "Cancelled.")
		return nil
	}

	return switchToWithIO(codexDir, current, candidates, out, selection)
}

func switchTo(codexDir, current string, candidates []candidate, selection string) error {
	return switchToWithIO(codexDir, current, candidates, os.Stdout, selection)
}

func switchToWithIO(codexDir, current string, candidates []candidate, out io.Writer, selection string) error {
	if len(candidates) == 0 {
		return fmt.Errorf("no auth.json.* files found in %s", codexDir)
	}

	target, err := resolveTarget(candidates, selection)
	if err != nil {
		return err
	}

	if current == target.Suffix {
		fmt.Fprintf(out, "Already using: %s\n", target.Suffix)
		return nil
	}

	if err := replaceAuthFile(codexDir, target.Path); err != nil {
		return err
	}

	fmt.Fprintf(out, "Switched auth to: %s\n", target.Suffix)
	return nil
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

	var reader *bufio.Reader
	if interactive {
		reader = bufio.NewReader(in)
	}

	for {
		targetPath := filepath.Join(codexDir, "auth.json."+suffix)
		if _, err := os.Stat(targetPath); err == nil {
			switch {
			case force:
				if err := copyFileToTarget(activePath, targetPath, true); err != nil {
					return err
				}
				fmt.Fprintf(out, "Overwrote auth alias: %s\n", suffix)
				return nil
			case !interactive:
				return fmt.Errorf("auth alias already exists: %s (use --force to overwrite or choose a different alias)", filepath.Base(targetPath))
			default:
				nextSuffix, overwrite, err := promptForSaveConflict(reader, out, targetPath)
				if err != nil {
					return err
				}
				if overwrite {
					if err := copyFileToTarget(activePath, targetPath, true); err != nil {
						return err
					}
					fmt.Fprintf(out, "Overwrote auth alias: %s\n", suffix)
					return nil
				}
				if nextSuffix == "" {
					continue
				}
				suffix = nextSuffix
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", targetPath, err)
		}

		if err := copyFileToTarget(activePath, targetPath, false); err != nil {
			return err
		}

		fmt.Fprintf(out, "Saved current auth as: %s\n", suffix)
		return nil
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
