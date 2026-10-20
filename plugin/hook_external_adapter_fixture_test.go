package plugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type adapterTreeEntry struct {
	Mode    os.FileMode
	Content string
}

// No ignored paths: tools, HOME, project, config, data and TEMP are all included.
func adapterTree(t *testing.T, root string) map[string]adapterTreeEntry {
	t.Helper()
	result := map[string]adapterTreeEntry{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content := ""
		if info.Mode()&os.ModeSymlink != 0 {
			content, err = os.Readlink(path)
		} else if info.Mode().IsRegular() {
			var data []byte
			data, err = os.ReadFile(path)
			content = string(data)
		}
		if err != nil {
			return err
		}
		result[path] = adapterTreeEntry{info.Mode(), content}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func adapterWrite(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// The only stubbed CLI actions are read-only setup/protocol conveniences. Every
// project inspection and session action executes the current native binary.
func adapterEnvironment(t *testing.T, root, binary, port, originalURL string, unset bool) []string {
	t.Helper()
	adapterWrite(t, filepath.Join(root, "inspection.jsonl"), "", 0600)
	adapterWrite(t, filepath.Join(root, "native-input.jsonl"), "", 0600)
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{bin, filepath.Join(root, "home"), filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "temp")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(filepath.ToSlash(s), "'", "'\"'\"'") + "'" }
	for _, tool := range []string{"cat", "dirname", "jq", "git", "bash"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("required %s: %v", tool, err)
		}
		adapterWrite(t, filepath.Join(bin, tool), "#!/bin/sh\nexec "+quote(path)+" \"$@\"\n", 0700)
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	adapterWrite(t, filepath.Join(bin, "curl"), "#!/bin/sh\nexec "+quote(curl)+" --disable --noproxy '*' --proxy '' --proto '=http' --globoff \"$@\"\n", 0700)
	adapterWrite(t, filepath.Join(bin, "engram"), `#!/bin/sh
case "$1:$2" in
 instance-id:) printf '%s\n' fixture-instance; exit 0 ;;
 protocol-mode:claude-code) printf '%s\n' full; exit 0 ;;
 hook:resolve-hook-project)
  input=$(cat)
  printf '%s\n' "$input" >> `+quote(filepath.Join(root, "inspection.jsonl"))+`
  printf '%s' "$input" | `+quote(binary)+` "$@"; exit $? ;;
 hook:codex-register|hook:codex-resolve)
  input=$(cat)
  printf '%s\n' "$input" >> `+quote(filepath.Join(root, "native-input.jsonl"))+`
  printf '%s' "$input" | `+quote(binary)+` "$@"; exit $? ;;
 *) printf '%s\n' 'unexpected fixture CLI action' >&2; exit 97 ;;
esac
`, 0700)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + bin + string(os.PathListSeparator) + filepath.Dir(git), "HOME=" + filepath.Join(root, "home"), "USERPROFILE=" + filepath.Join(root, "home"),
		"APPDATA=" + filepath.Join(root, "config"), "LOCALAPPDATA=" + filepath.Join(root, "config"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"ENGRAM_DATA_DIR=" + filepath.Join(root, "data"), "ENGRAM_PORT=" + port, "ENGRAM_SOCKET=",
		"TMPDIR=" + filepath.Join(root, "temp"), "TMP=" + filepath.Join(root, "temp"), "TEMP=" + filepath.Join(root, "temp"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"), "GIT_CEILING_DIRECTORIES=" + root,
	}
	if runtime.GOOS == "windows" {
		// PowerShell initializes these standard profile directories; keep their
		// empty baseline inside the complete fixture snapshot, not an exclusion.
		if err := os.MkdirAll(filepath.Join(root, "home", "AppData", "Roaming"), 0700); err != nil {
			t.Fatal(err)
		}
		// Restore private precedence after the real Git/bin/bash launcher starts.
		// cmd.Dir=root supplies POSIX PWD. Precreate before the full-tree snapshot;
		// child shells inherit PATH without sourcing this fixture startup again.
		startup := filepath.Join(root, "fixture-startup.sh")
		adapterWrite(t, startup, "PATH=\"$PWD/bin:$PATH\"\nexport PATH\nunset BASH_ENV\n", 0600)
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"), "BASH_ENV="+filepath.ToSlash(startup))
	}
	if !unset {
		env = append(env, "ENGRAM_URL="+originalURL)
	}
	return env
}

func runExternalAdapter(t *testing.T, bash, agent, script, root, input string, env []string, windows bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	hook := filepath.Join(repoRoot(t), "plugin", agent, "scripts", script)
	cmd := exec.CommandContext(ctx, bash, hook)
	if windows {
		powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		cmd = exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(repoRoot(t), "plugin", agent, "scripts", "run-bash-hook.ps1"), hook)
	}
	cmd.Dir, cmd.Env, cmd.Stdin = root, env, strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	t.Logf("exit=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	if err != nil || stderr.Len() != 0 {
		t.Errorf("want exit 0 and empty stderr")
	}
	return stdout.String()
}

func adapterJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(fmt.Errorf("encode fixture: %w", err))
	}
	return string(data)
}
