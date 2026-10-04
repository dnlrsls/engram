package plugin_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPluginHookShebangsArePortable(t *testing.T) {
	root := repoRoot(t)

	for _, plugin := range []string{"claude-code", "codex"} {
		wantScripts := 6
		if plugin == "claude-code" {
			wantScripts = 7 // Includes the shared bridge ownership helper.
		}
		scriptsDir := filepath.Join(root, "plugin", plugin, "scripts")
		entries, err := os.ReadDir(scriptsDir)
		if err != nil {
			t.Fatalf("read %s scripts directory: %v", plugin, err)
		}

		var scripts []string
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".sh" {
				scripts = append(scripts, filepath.Join(scriptsDir, entry.Name()))
			}
		}
		if len(scripts) != wantScripts {
			t.Fatalf("%s scripts directory contains %d .sh files, want %d", plugin, len(scripts), wantScripts)
		}

		for _, script := range scripts {
			data, err := os.ReadFile(script)
			if err != nil {
				t.Fatalf("read %s: %v", script, err)
			}
			if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
				t.Errorf("%s must not start with a UTF-8 BOM", script)
			}
			if bytes.Contains(data, []byte("\r")) {
				t.Errorf("%s must use LF line endings", script)
			}

			firstLine, _, _ := bytes.Cut(data, []byte("\n"))
			if string(firstLine) != "#!/usr/bin/env bash" {
				t.Errorf("%s first line = %q, want %q", script, firstLine, "#!/usr/bin/env bash")
			}
		}
	}
}
