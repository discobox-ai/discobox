package sandboxconfig

import (
	"reflect"
	"testing"

	"github.com/obot-platform/discobox/harness"
)

func TestEffective_SingleWriterFields(t *testing.T) {
	doc := Document{
		Runtime: RuntimeLayer{
			SandboxID: "sbx_1",
			Provider:  Provider{Kind: "discobox-pool", ProjectID: "proj_1"},
			Sources:   []Source{{Slug: "main", Target: "/workspace"}},
			Model:     "claude",
			User:      User{Name: "sandbox"},
			Git:       GitIdentity{UserName: "Ada Lovelace", UserEmail: "ada@example.com"},
		},
		Image: ImageLayer{HarnessID: "h1", HarnessName: "Harness One"},
	}
	cfg, prov := Effective(doc)

	if cfg.Git != (GitIdentity{UserName: "Ada Lovelace", UserEmail: "ada@example.com"}) {
		t.Errorf("Git = %+v, want the runtime layer's identity copied through", cfg.Git)
	}

	if cfg.SandboxID != "sbx_1" {
		t.Errorf("SandboxID = %q, want sbx_1", cfg.SandboxID)
	}
	if cfg.Provider.ProjectID != "proj_1" {
		t.Errorf("Provider.ProjectID = %q", cfg.Provider.ProjectID)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].Slug != "main" {
		t.Errorf("Sources = %+v", cfg.Sources)
	}
	if cfg.Model != "claude" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.Harness.ID != "h1" || cfg.Harness.Name != "Harness One" {
		t.Errorf("Harness = %+v", cfg.Harness)
	}
	if cfg.APIVersion != APIVersion {
		t.Errorf("APIVersion = %q, want %q", cfg.APIVersion, APIVersion)
	}
	if prov.Runtime.SandboxID != "sbx_1" {
		t.Errorf("provenance runtime not preserved: %+v", prov.Runtime)
	}
	if prov.Project != nil {
		t.Errorf("provenance project should be nil when Document.Project is nil")
	}
}

func TestEffective_RunCommandOverrideGrant(t *testing.T) {
	cases := []struct {
		name    string
		project *ProjectLayer
		want    []string
	}{
		{"no project layer, image wins", nil, []string{"image-cmd"}},
		{"project layer empty command, image wins", &ProjectLayer{}, []string{"image-cmd"}},
		{"project layer sets command, project wins", &ProjectLayer{RunCommand: []string{"project-cmd"}}, []string{"project-cmd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := Document{
				Image:   ImageLayer{RunCommand: []string{"image-cmd"}},
				Project: tc.project,
			}
			cfg, _ := Effective(doc)
			if !reflect.DeepEqual(cfg.Harness.RunCommand, tc.want) {
				t.Errorf("RunCommand = %v, want %v", cfg.Harness.RunCommand, tc.want)
			}
		})
	}
}

func TestEffective_RelaunchCommandOverrideGrant(t *testing.T) {
	doc := Document{
		Image:   ImageLayer{RelaunchCommand: []string{"image-relaunch"}},
		Project: &ProjectLayer{RelaunchCommand: []string{"project-relaunch"}},
	}
	cfg, _ := Effective(doc)
	if !reflect.DeepEqual(cfg.Harness.RelaunchCommand, []string{"project-relaunch"}) {
		t.Errorf("RelaunchCommand = %v", cfg.Harness.RelaunchCommand)
	}
}

func TestEffective_ConfigCommandIsImageOnly(t *testing.T) {
	doc := Document{
		Image: ImageLayer{ConfigCommand: []string{"configure"}},
	}
	cfg, _ := Effective(doc)
	if !reflect.DeepEqual(cfg.Harness.ConfigCommand, []string{"configure"}) {
		t.Errorf("ConfigCommand = %v", cfg.Harness.ConfigCommand)
	}
}

func TestEffective_EnvAdditiveDefault(t *testing.T) {
	doc := Document{
		Image:   ImageLayer{Env: map[string]string{"A": "image-a", "B": "image-b"}},
		Runtime: RuntimeLayer{Env: map[string]string{"A": "runtime-a", "C": "runtime-c"}},
	}
	cfg, _ := Effective(doc)
	want := map[string]string{"A": "runtime-a", "B": "image-b", "C": "runtime-c"}
	if !reflect.DeepEqual(cfg.Env, want) {
		t.Errorf("Env = %v, want %v", cfg.Env, want)
	}
}

func TestEffective_FilesOverlayByPath(t *testing.T) {
	doc := Document{
		Image: ImageLayer{Files: []File{
			{Path: "/home/user/.a", Content: "image-a"},
			{Path: "/home/user/.b", Content: "image-b"},
		}},
		Runtime: RuntimeLayer{Files: []File{
			{Path: "/home/user/.a", Content: "runtime-a"}, // overlay replaces
			{Path: "/home/user/.c", Content: "runtime-c"}, // new path appends
		}},
		Project: &ProjectLayer{FilesAdd: []File{
			{Path: "/home/user/.d", Content: "project-d"}, // new path, appended
			{Path: "/home/user/.a", Content: "project-a"}, // conflicting path, ignored
		}},
	}
	cfg, _ := Effective(doc)
	want := []File{
		{Path: "/home/user/.a", Content: "runtime-a"},
		{Path: "/home/user/.b", Content: "image-b"},
		{Path: "/home/user/.c", Content: "runtime-c"},
		{Path: "/home/user/.d", Content: "project-d"},
	}
	if !reflect.DeepEqual(cfg.Files, want) {
		t.Errorf("Files = %+v, want %+v", cfg.Files, want)
	}
}

func TestEffective_VolumesAreImageOnly(t *testing.T) {
	doc := Document{
		Image: ImageLayer{Volumes: []harness.Volume{{Path: "/data", Volume: harness.VolumeData}}},
	}
	cfg, _ := Effective(doc)
	if len(cfg.Volumes) != 1 || cfg.Volumes[0].Path != "/data" {
		t.Errorf("Volumes = %+v", cfg.Volumes)
	}
}

func TestEffective_WorkingDirectorySubpathIsProjectOnly(t *testing.T) {
	doc := Document{Project: &ProjectLayer{WorkingDirectorySubpath: "sub/dir"}}
	cfg, _ := Effective(doc)
	if cfg.WorkingDirectorySubpath != "sub/dir" {
		t.Errorf("WorkingDirectorySubpath = %q", cfg.WorkingDirectorySubpath)
	}

	docNoProject := Document{}
	cfgNoProject, _ := Effective(docNoProject)
	if cfgNoProject.WorkingDirectorySubpath != "" {
		t.Errorf("WorkingDirectorySubpath = %q, want empty", cfgNoProject.WorkingDirectorySubpath)
	}
}

func TestEffective_HarnessModeIsRuntimeOnly(t *testing.T) {
	doc := Document{Runtime: RuntimeLayer{HarnessMode: "config"}}
	cfg, _ := Effective(doc)
	if cfg.HarnessMode != "config" {
		t.Errorf("HarnessMode = %q", cfg.HarnessMode)
	}
}
