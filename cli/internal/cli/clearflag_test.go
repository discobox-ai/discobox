package cli

import "testing"

// --clear-file sends an empty list, which is what empties the harness's files;
// leaving both flags out sends none.
func TestHarnessUpdateClearFileSendsAnEmptyList(t *testing.T) {
	for _, tc := range []struct {
		args []string
		sent bool
	}{
		{args: nil, sent: false},
		{args: []string{"--clear-file"}, sent: true},
	} {
		cmd := (&App{}).newHarnessUpdateCommand()
		if err := cmd.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		body, err := updateHarnessBody(cmd, harnessUpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		files, sent := body.Files.Get()
		if sent != tc.sent || len(files) != 0 {
			t.Fatalf("%v: files = %v sent %v, want sent %v and empty", tc.args, files, sent, tc.sent)
		}
	}
}
