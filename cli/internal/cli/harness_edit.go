package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// harnessFile is the file a sandbox is given at one path, with the set it
// comes from and the sets beneath it that it hides there, topmost first.
type harnessFile struct {
	apimodel.HarnessConfigFile
	Source  console.HarnessFileSource
	Shadows []console.HarnessFileSource
}

// harnessEffectiveFiles lists every path of a config once, as a sandbox is
// given it (ADR 26-11-04-028 §2): a person's files first, then the configure
// flow's, then the bundle's.
func harnessEffectiveFiles(cfg *apimodel.HarnessConfig) []harnessFile {
	var files []harnessFile
	index := map[string]int{}
	for _, set := range []struct {
		source console.HarnessFileSource
		files  []apimodel.HarnessConfigFile
	}{
		{console.HarnessFileOverride, cfg.FileOverrides.Or(nil)},
		{console.HarnessFileSaved, cfg.ConfiguredFiles.Or(nil)},
		{console.HarnessFileFromBundle, cfg.Files.Or(nil)},
	} {
		for _, file := range set.files {
			if i, ok := index[file.Path]; ok {
				files[i].Shadows = append(files[i].Shadows, set.source)
				continue
			}
			index[file.Path] = len(files)
			files = append(files, harnessFile{HarnessConfigFile: file, Source: set.source})
		}
	}
	return files
}

// findHarnessFile is the file a sandbox is given at path.
func findHarnessFile(cfg *apimodel.HarnessConfig, path string) (harnessFile, bool) {
	for _, file := range harnessEffectiveFiles(cfg) {
		if file.Path == path {
			return file, true
		}
	}
	return harnessFile{}, false
}

func (a *App) newHarnessEditCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "edit HARNESS PATH",
		Short: "Edit one of a harness config's files in your editor",
		Long: `Open one of a harness config's files in $VISUAL/$EDITOR and save the result back.

PATH is the file's path as shown by "harnesses get" or the console's harnesses
screen, which is what "discobox configure" opens. The file edited is the one a
discobox is given at PATH. A file saved from a discobox's harness change is
edited in place, and stays a saved file. A file from the harness's bundle is saved as
your override of it, so the edit survives the bundle being updated; remove it
with "harnesses update HARNESS --remove-file PATH".`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completeHarnessConfigs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			harnessRes, err := client.GetHarnessConfig(cmd.Context(), apiclientgen.GetHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
			if err != nil {
				return err
			}
			harness, err := expectResponse[apimodel.HarnessConfig](harnessRes)
			if err != nil {
				return err
			}
			changed, err := a.editHarnessFile(cmd.Context(), client, projectID, harness, args[1],
				runEditor(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()))
			if err != nil {
				return err
			}
			if !changed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s unchanged\n", args[1])
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n", args[1])
			return err
		},
	}
}

// editHarnessFile has the named config file edited — written to a temp file
// that edit opens in an editor and returns from once the person is finished —
// and, when the content changed, replaces the file's bucket via the update
// API. It reports whether an update was made.
func (a *App) editHarnessFile(ctx context.Context, client *apiclientgen.Client, projectID string, cfg *apimodel.HarnessConfig, path string,
	edit func(ctx context.Context, file string) error,
) (bool, error) {
	file, ok := findHarnessFile(cfg, path)
	if !ok {
		files := harnessEffectiveFiles(cfg)
		if len(files) == 0 {
			return false, fmt.Errorf("%s has no files to edit", harnessDisplayName(*cfg))
		}
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		return false, fmt.Errorf("no file %q; available: %s", path, strings.Join(paths, ", "))
	}

	edited, changed, err := editInEditor(ctx, file.Path, file.Content, edit)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	file.Content = edited

	// A saved file is edited in the saved set, and stays a saved file.
	// A bundle's file is the bundle's, re-read whenever it moves, so an edit
	// to it is the person's override, as an edit to their own file is.
	body := &apimodel.UpdateHarnessConfigBody{Revision: cfg.Revision.Or(0)}
	if file.Source == console.HarnessFileSaved {
		body.SetConfiguredFiles(apiclientgen.NewOptNilHarnessConfigFileArray(withHarnessFile(cfg.ConfiguredFiles.Or(nil), file.HarnessConfigFile)))
	} else {
		body.SetFileOverrides(apiclientgen.NewOptNilHarnessConfigFileArray(withHarnessFile(cfg.FileOverrides.Or(nil), file.HarnessConfigFile)))
	}
	res, err := client.UpdateHarnessConfig(ctx, body, apiclientgen.UpdateHarnessConfigParams{ProjectId: projectID, HarnessConfigId: cfg.ID})
	if err != nil {
		return false, err
	}
	if _, err := expectResponse[apimodel.HarnessConfig](res); err != nil {
		return false, err
	}
	return true, nil
}

// withHarnessFile is set with file in place of the entry at its path, or
// added after the others.
func withHarnessFile(set []apimodel.HarnessConfigFile, file apimodel.HarnessConfigFile) []apimodel.HarnessConfigFile {
	updated := append([]apimodel.HarnessConfigFile{}, set...)
	for i := range updated {
		if updated[i].Path == file.Path {
			updated[i] = file
			return updated
		}
	}
	return append(updated, file)
}

// editInEditor writes content to a temp file, has edit open it in an editor,
// and returns what the file holds once it returns and whether that differs.
// The temp file keeps the target's extension, when it is a text format's, so
// editors pick the right syntax mode (editExtension).
func editInEditor(ctx context.Context, path, content string, edit func(ctx context.Context, file string) error) (string, bool, error) {
	tmp, err := os.CreateTemp("", "discobox-edit-*"+editExtension(path))
	if err != nil {
		return "", false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", false, err
	}
	if err := tmp.Close(); err != nil {
		return "", false, err
	}
	if err := edit(ctx, tmpPath); err != nil {
		return "", false, err
	}

	editedBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", false, err
	}
	edited := string(editedBytes)
	return edited, !bytes.Equal(editedBytes, []byte(content)), nil
}

// editExtensions are the extensions an edited file keeps: text formats a
// desktop hands to a text editor. The path is the harness bundle's to choose,
// and on Linux the desktop app opens the file with the handler for its type:
// an extension outside these (a script's, a launcher's, one carrying shell
// metacharacters) would let a bundle choose a program to run on this machine,
// and one a desktop shows rather than edits (.xml, .md, .html: a browser or a
// viewer) would leave nothing to save, so either is written as .txt.
var editExtensions = map[string]bool{
	".json": true, ".jsonc": true, ".json5": true, ".toml": true, ".yaml": true, ".yml": true,
	".txt": true, ".ini": true, ".conf": true, ".cfg": true, ".env": true,
}

// editExtension is the extension a file at path is edited under: its own
// when it is one of editExtensions, else .txt.
func editExtension(path string) string {
	if ext := strings.ToLower(filepath.Ext(path)); editExtensions[ext] {
		return ext
	}
	return ".txt"
}

// runEditor is an edit run in the user's editor ($VISUAL, $EDITOR) on this
// terminal, returning once it exits.
func runEditor(stdin io.Reader, stdout, stderr io.Writer) func(ctx context.Context, file string) error {
	return func(ctx context.Context, file string) error {
		argv := append(editorCommand(), file)
		//nolint:gosec // Launching the user's own $VISUAL/$EDITOR is the point.
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Stdin = stdin
		if stdin == nil {
			cmd.Stdin = os.Stdin
		}
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("editor %s: %w", argv[0], err)
		}
		return nil
	}
}

// editorCommand resolves the editor argv from $VISUAL then $EDITOR, splitting
// on whitespace so values like "code --wait" work, falling back to the
// platform's default editor.
func editorCommand() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			return strings.Fields(value)
		}
	}
	return []string{defaultEditor()}
}

// defaultEditor is what to run when neither variable is set. Windows has no
// vi, but Windows 11 ships edit.exe — a terminal editor that behaves the way
// this code expects — with notepad as the backstop for builds without it.
func defaultEditor() string {
	if runtime.GOOS != "windows" {
		return "vi"
	}
	for _, candidate := range []string{"edit.exe", "notepad.exe"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return "notepad.exe"
}
