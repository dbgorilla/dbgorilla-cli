package ide

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyEntries_VSCode(t *testing.T) {
	setup := func(t *testing.T) (proj, user string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("COPILOT_HOME", "")
		t.Chdir(t.TempDir())
		setGOOS(t, "linux")
		cwd, _ := os.Getwd()
		return filepath.Join(cwd, ".vscode", "mcp.json"),
			filepath.Join(home, ".config", "Code", "User", "mcp.json")
	}

	t.Run("none when no old file exists", func(t *testing.T) {
		setup(t)
		if got := LegacyEntries(&VSCode{}); len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("finds entries under servers in both old files", func(t *testing.T) {
		proj, user := setup(t)
		writeFile(t, proj, `{"servers":{"dbgorilla":{"type":"http"}}}`)
		writeFile(t, user, `{"servers":{"other":{},"dbgorilla":{}}}`)
		got := LegacyEntries(&VSCode{})
		if len(got) != 2 || got[0] != proj || got[1] != user {
			t.Errorf("got %v, want [%s %s]", got, proj, user)
		}
	})

	t.Run("ignores old files without our entry", func(t *testing.T) {
		proj, _ := setup(t)
		writeFile(t, proj, `{"servers":{"other":{}},"inputs":[]}`)
		if got := LegacyEntries(&VSCode{}); len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("counts a commented file that names the server", func(t *testing.T) {
		_, user := setup(t)
		writeFile(t, user, "// mine\n{\"servers\":{\"dbgorilla\":{}}}\n")
		if got := LegacyEntries(&VSCode{}); len(got) != 1 || got[0] != user {
			t.Errorf("got %v, want [%s]", got, user)
		}
	})

	t.Run("adapters without legacy files report none", func(t *testing.T) {
		setup(t)
		if got := LegacyEntries(&Cursor{}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}
