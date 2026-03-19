package main

import (
	"bufio"
	"bytes"
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
)

type candidate struct {
	Name   string
	Suffix string
	Path   string
}

func main() {
	listOnly := flag.Bool("list", false, "show the current auth and all available auth.json.* files")
	useValue := flag.String("use", "", "switch to auth.json.<suffix> or the menu index")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  %s\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "  %s --list\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "  %s --use <suffix-or-index>\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(flag.CommandLine.Output(), "\nEnvironment:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  CODEX_HOME  Override the auth directory. Defaults to %s\n", defaultCodexHomeHint())
	}
	flag.Parse()

	if flag.NArg() != 0 {
		exitf("unknown argument: %s", strings.Join(flag.Args(), " "))
	}

	codexDir, err := codexHome()
	if err != nil {
		exitErr(err)
	}

	candidates, err := loadCandidates(codexDir)
	if err != nil {
		exitErr(err)
	}

	current, err := currentSuffix(codexDir, candidates)
	if err != nil {
		exitErr(err)
	}

	switch {
	case *listOnly:
		printStatus(codexDir, current, candidates)
	case *useValue != "":
		if err := switchTo(codexDir, current, candidates, *useValue); err != nil {
			exitErr(err)
		}
	default:
		if err := interactiveMode(codexDir, current, candidates); err != nil {
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

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Suffix < candidates[j].Suffix
	})

	return candidates, nil
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

func printStatus(codexDir, current string, candidates []candidate) {
	fmt.Printf("Auth dir: %s\n", codexDir)
	fmt.Printf("Current auth: %s\n", current)

	if len(candidates) == 0 {
		fmt.Println("Available auth files: none")
		return
	}

	fmt.Println("Available auth files:")
	for i, candidate := range candidates {
		marker := " "
		if candidate.Suffix == current {
			marker = "*"
		}
		fmt.Printf("  [%d] %s %s\n", i+1, marker, candidate.Suffix)
	}
}

func interactiveMode(codexDir, current string, candidates []candidate) error {
	printStatus(codexDir, current, candidates)

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

func replaceAuthFile(codexDir, sourcePath string) error {
	activePath := filepath.Join(codexDir, "auth.json")

	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer source.Close()

	tmp, err := os.CreateTemp(codexDir, "auth.json.tmp.*")
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

	if err := os.Rename(tmpPath, activePath); err != nil {
		if removeErr := os.Remove(activePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("replace %s: rename failed (%v), remove failed (%w)", activePath, err, removeErr)
		}
		if renameErr := os.Rename(tmpPath, activePath); renameErr != nil {
			return fmt.Errorf("replace %s after remove: %w", activePath, renameErr)
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
