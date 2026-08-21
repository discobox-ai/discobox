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

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// harnessFileBucket names which HarnessConfig file set a file lives in, since
// the update API replaces each set wholesale and the edit must go back into
// the set it came from.
type harnessFileBucket string

const (
	harnessFileBucketDeclared   harnessFileBucket = "declared"
	harnessFileBucketConfigured harnessFileBucket = "configured"
)

// harnessFileRef identifies one editable file of a harness config.
type harnessFileRef struct {
	Path   string
	Bucket harnessFileBucket
}

// harnessFileRefs lists a config's editable files, configure-flow files first
// since those overlay the image-declared set when a sandbox is resolved.
func harnessFileRefs(cfg *apimodel.HarnessConfig) []harnessFileRef {
	var refs []harnessFileRef
	for _, file := range cfg.ConfiguredFiles.Or(nil) {
		refs = append(refs, harnessFileRef{Path: file.Path, Bucket: harnessFileBucketConfigured})
	}
	for _, file := range cfg.Files.Or(nil) {
		refs = append(refs, harnessFileRef{Path: file.Path, Bucket: harnessFileBucketDeclared})
	}
	return refs
}

// findHarnessFile resolves path to the file entry that is effective for the
// sandbox: the configured overlay wins over the image-declared set.
func findHarnessFile(cfg *apimodel.HarnessConfig, path string) (harnessFileRef, bool) {
	for _, ref := range harnessFileRefs(cfg) {
		if ref.Path == path {
			return ref, true
		}
	}
	return harnessFileRef{}, false
}

func (a *App) newHarnessEditCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "edit HARNESS PATH",
		Short: "Edit one of a harness config's files in your editor",
		Long: `Open one of a harness config's files in $VISUAL/$EDITOR and save the result back.

PATH is the file's path as shown by "harnesses get" or the launcher's harnesses
screen, which is what "discobox configure" opens.
Files written by the configure flow are edited in place of the configured set;
image-declared files are edited in the declared set.`,
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
				cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
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

// editHarnessFile opens the named config file in the user's editor and, when
// the content changed, replaces the file's bucket via the update API. It
// reports whether an update was made.
func (a *App) editHarnessFile(ctx context.Context, client *apiclientgen.Client, projectID string, cfg *apimodel.HarnessConfig, path string,
	stdin io.Reader, stdout, stderr io.Writer,
) (bool, error) {
	ref, ok := findHarnessFile(cfg, path)
	if !ok {
		refs := harnessFileRefs(cfg)
		if len(refs) == 0 {
			return false, fmt.Errorf("%s has no files to edit", harnessDisplayName(*cfg))
		}
		paths := make([]string, 0, len(refs))
		for _, r := range refs {
			paths = append(paths, r.Path)
		}
		return false, fmt.Errorf("no file %q; available: %s", path, strings.Join(paths, ", "))
	}

	files := cfg.Files.Or(nil)
	if ref.Bucket == harnessFileBucketConfigured {
		files = cfg.ConfiguredFiles.Or(nil)
	}
	index := -1
	for i := range files {
		if files[i].Path == ref.Path {
			index = i
			break
		}
	}
	if index < 0 {
		return false, fmt.Errorf("no file %q", path)
	}

	edited, changed, err := editInEditor(ctx, ref.Path, files[index].Content, stdin, stdout, stderr)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}

	updated := make([]apimodel.HarnessConfigFile, len(files))
	copy(updated, files)
	updated[index].Content = edited

	body := &apimodel.UpdateHarnessConfigBody{}
	if ref.Bucket == harnessFileBucketConfigured {
		body.SetConfiguredFiles(apiclientgen.NewOptNilHarnessConfigFileArray(updated))
	} else {
		body.SetFiles(apiclientgen.NewOptNilHarnessConfigFileArray(updated))
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

// editInEditor runs the user's editor on content in a temp file and returns
// the result and whether it differs. The temp file keeps the target's
// extension so editors pick the right syntax mode.
func editInEditor(ctx context.Context, path, content string, stdin io.Reader, stdout, stderr io.Writer) (string, bool, error) {
	tmp, err := os.CreateTemp("", "discobox-edit-*"+filepath.Ext(path))
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

	argv := append(editorCommand(), tmpPath)
	//nolint:gosec // Launching the user's own $VISUAL/$EDITOR is the point.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	if stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("editor %s: %w", argv[0], err)
	}

	editedBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", false, err
	}
	edited := string(editedBytes)
	return edited, !bytes.Equal(editedBytes, []byte(content)), nil
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
