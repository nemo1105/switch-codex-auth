package main

import (
	"bufio"
	"bytes"
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
}

type authMetadata struct {
	ModTime        time.Time
	AccessTime     time.Time
	HasAccessTime  bool
	LastRefresh    time.Time
	HasLastRefresh bool
}

var nowFunc = time.Now

func main() {
	listOnly := flag.Bool("list", false, "show the current auth and all available auth.json.* files")
	useValue := flag.String("use", "", "switch to auth.json.<suffix> or the menu index")
	saveValue := flag.String("save", "", "copy the current auth.json to auth.json.<suffix>")
	refreshValue := flag.Bool("refresh", false, "refresh all refreshable auth.json.* files")
	var force bool
	flag.BoolVar(&force, "f", false, "overwrite an existing auth alias when used with --save")
	flag.BoolVar(&force, "force", false, "overwrite an existing auth alias when used with --save")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  %s\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "  %s --list\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "  %s --use <suffix-or-index>\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "  %s --save <suffix> [-f|--force]\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "  %s --refresh\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "\nFlags:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  -f, --force  overwrite an existing auth alias when used with --save\n")
		fmt.Fprintf(flag.CommandLine.Output(), "\nEnvironment:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  CODEX_HOME  Override the auth directory. Defaults to %s\n", defaultCodexHomeHint())
	}
	flag.Parse()

	if flag.NArg() != 0 {
		exitf("unknown argument: %s", strings.Join(flag.Args(), " "))
	}

	actionCount, err := countSelectedActions(*listOnly, *useValue, *saveValue, *refreshValue)
	if err != nil {
		exitErr(err)
	}
	if err := validateSaveOptions(*saveValue, force); err != nil {
		exitErr(err)
	}

	codexDir, err := codexHome()
	if err != nil {
		exitErr(err)
	}

	var candidates []candidate
	if *listOnly || *useValue != "" || *refreshValue || actionCount == 0 {
		candidates, err = loadCandidates(codexDir)
		if err != nil {
			exitErr(err)
		}
	}

	switch {
	case *listOnly:
		currentMetadata, err := loadCurrentAuthMetadata(codexDir)
		if err != nil {
			exitErr(err)
		}

		current, err := currentSuffix(codexDir, candidates)
		if err != nil {
			exitErr(err)
		}

		printStatus(os.Stdout, codexDir, current, currentMetadata, candidates)
	case *useValue != "":
		current, err := currentSuffix(codexDir, candidates)
		if err != nil {
			exitErr(err)
		}

		if err := switchTo(codexDir, current, candidates, *useValue); err != nil {
			exitErr(err)
		}
	case *saveValue != "":
		if err := saveCurrentAs(codexDir, *saveValue, force); err != nil {
			exitErr(err)
		}
	case *refreshValue:
		if err := refreshAuthAliases(os.Stdout, codexDir, candidates); err != nil {
			exitErr(err)
		}
	default:
		currentMetadata, err := loadCurrentAuthMetadata(codexDir)
		if err != nil {
			exitErr(err)
		}

		current, err := currentSuffix(codexDir, candidates)
		if err != nil {
			exitErr(err)
		}

		if err := interactiveMode(codexDir, current, currentMetadata, candidates); err != nil {
			exitErr(err)
		}
	}
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
	activeBytes, err := os.ReadFile(activePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "none", nil
		}
		return "", fmt.Errorf("read %s: %w", activePath, err)
	}

	for _, candidate := range candidates {
		candidateBytes, err := os.ReadFile(candidate.Path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", candidate.Path, err)
		}
		if bytes.Equal(activeBytes, candidateBytes) {
			return candidate.Suffix, nil
		}
	}

	return "custom/unmatched", nil
}

func printStatus(w io.Writer, codexDir, current string, currentMetadata *authMetadata, candidates []candidate) {
	fmt.Fprintf(w, "Auth dir: %s\n", codexDir)
	fmt.Fprintf(w, "Current auth: %s\n", current)

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
		index       string
		current     string
		suffix      string
		modified    string
		accessed    string
		lastRefresh string
	}

	rows := make([]candidateRow, 0, len(candidates))
	indexWidth := len("#")
	currentWidth := len("Current")
	suffixWidth := len("Suffix")
	modifiedWidth := len("Modified")
	accessedWidth := len("Accessed")
	lastRefreshWidth := len("Last refresh")

	for i, candidate := range candidates {
		marker := ""
		if candidate.Suffix == current {
			marker = "*"
		}

		row := candidateRow{
			index:       strconv.Itoa(i + 1),
			current:     marker,
			suffix:      candidate.Suffix,
			modified:    formatDisplayTime(candidate.Metadata.ModTime, true),
			accessed:    formatDisplayTime(candidate.Metadata.AccessTime, candidate.Metadata.HasAccessTime),
			lastRefresh: formatDisplayTime(candidate.Metadata.LastRefresh, candidate.Metadata.HasLastRefresh),
		}
		rows = append(rows, row)

		if len(row.index) > indexWidth {
			indexWidth = len(row.index)
		}
		if len(row.suffix) > suffixWidth {
			suffixWidth = len(row.suffix)
		}
		if len(row.modified) > modifiedWidth {
			modifiedWidth = len(row.modified)
		}
		if len(row.accessed) > accessedWidth {
			accessedWidth = len(row.accessed)
		}
		if len(row.lastRefresh) > lastRefreshWidth {
			lastRefreshWidth = len(row.lastRefresh)
		}
	}

	fmt.Fprintf(
		w,
		"  %-*s %-*s %-*s %-*s %-*s %-*s\n",
		indexWidth,
		"#",
		currentWidth,
		"Current",
		suffixWidth,
		"Suffix",
		modifiedWidth,
		"Modified",
		accessedWidth,
		"Accessed",
		lastRefreshWidth,
		"Last refresh",
	)

	for _, row := range rows {
		fmt.Fprintf(
			w,
			"  %-*s %-*s %-*s %-*s %-*s %-*s\n",
			indexWidth,
			row.index,
			currentWidth,
			row.current,
			suffixWidth,
			row.suffix,
			modifiedWidth,
			row.modified,
			accessedWidth,
			row.accessed,
			lastRefreshWidth,
			row.lastRefresh,
		)
	}

	fmt.Fprintln(w, "Hint: newer last_refresh and access times usually mean more recent activity, but they do not guarantee remaining quota or validity.")
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
	printStatus(os.Stdout, codexDir, current, currentMetadata, candidates)

	if len(candidates) == 0 {
		return fmt.Errorf("no auth.json.* files found in %s", codexDir)
	}

	fmt.Print("Choose auth file by number or suffix (Enter to cancel): ")
	reader := bufio.NewReader(os.Stdin)
	selection, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read selection: %w", err)
	}

	selection = strings.TrimSpace(selection)
	if selection == "" {
		fmt.Println("Cancelled.")
		return nil
	}

	return switchTo(codexDir, current, candidates, selection)
}

func switchTo(codexDir, current string, candidates []candidate, selection string) error {
	if len(candidates) == 0 {
		return fmt.Errorf("no auth.json.* files found in %s", codexDir)
	}

	target, err := resolveTarget(candidates, selection)
	if err != nil {
		return err
	}

	if current == target.Suffix {
		fmt.Printf("Already using: %s\n", target.Suffix)
		return nil
	}

	if err := replaceAuthFile(codexDir, target.Path); err != nil {
		return err
	}

	fmt.Printf("Switched auth to: %s\n", target.Suffix)
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

func validateSaveOptions(saveValue string, force bool) error {
	if force && saveValue == "" {
		return fmt.Errorf("use --force only with --save")
	}
	return nil
}

func countSelectedActions(listOnly bool, useValue, saveValue string, refresh bool) (int, error) {
	actionCount := 0
	if listOnly {
		actionCount++
	}
	if useValue != "" {
		actionCount++
	}
	if saveValue != "" {
		actionCount++
	}
	if refresh {
		actionCount++
	}
	if actionCount > 1 {
		return 0, fmt.Errorf("use only one of --list, --use, --save, or --refresh")
	}
	return actionCount, nil
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
