package ide

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillBodyIsUsable guards the two things a broken skill fails silently
// on: Claude reads the front matter to decide when to load it, so a skill
// without a name or description is inert, and a skill nobody reads is worse
// than none at all if it is long enough to be skipped.
func TestSkillBodyIsUsable(t *testing.T) {
	if !strings.HasPrefix(skillBody, "---\n") {
		t.Fatal("skill must open with YAML front matter")
	}
	end := strings.Index(skillBody[4:], "\n---\n")
	if end < 0 {
		t.Fatal("skill front matter is never closed")
	}
	front := skillBody[4 : 4+end]
	for _, key := range []string{"name: " + SkillName, "description:"} {
		if !strings.Contains(front, key) {
			t.Errorf("front matter is missing %q:\n%s", key, front)
		}
	}
	// The value of this skill is that it is short enough to always be worth
	// loading. If it grows into documentation, split it or cut it.
	if n := strings.Count(skillBody, "\n"); n > 40 {
		t.Errorf("skill is %d lines; keep it under 40", n)
	}
}

func TestSkillDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got, err := SkillDir(ScopeUser)
	if err != nil {
		t.Fatalf("user scope: %v", err)
	}
	if want := filepath.Join(home, ".claude", "skills", "dbgorilla"); got != want {
		t.Errorf("user scope = %q, want %q", got, want)
	}

	got, err = SkillDir(ScopeProject)
	if err != nil {
		t.Fatalf("project scope: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".claude", "skills", "dbgorilla")) {
		t.Errorf("project scope = %q, want a path under the working directory", got)
	}
	if strings.HasPrefix(got, home) {
		t.Errorf("project scope resolved into the home directory: %q", got)
	}
}

func TestInstallSkill_IsIdempotentAndSelfUpdating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".claude", "skills", "dbgorilla", "SKILL.md")

	res, err := InstallSkill(ScopeUser)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !res.Created || res.Path != path {
		t.Errorf("first install = %+v, want Created at %s", res, path)
	}
	if data, _ := os.ReadFile(path); string(data) != skillBody {
		t.Error("installed file does not match the shipped skill")
	}

	// Running setup-ide again must not churn the file or say anything new.
	res, err = InstallSkill(ScopeUser)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !res.NoOp {
		t.Errorf("second install = %+v, want a no-op", res)
	}

	// An older copy is replaced whole, not merged or left alone.
	if err := os.WriteFile(path, []byte("stale from an older release\n"), 0644); err != nil {
		t.Fatal(err)
	}
	res, err = InstallSkill(ScopeUser)
	if err != nil {
		t.Fatalf("third install: %v", err)
	}
	if !res.Updated {
		t.Errorf("third install = %+v, want Updated", res)
	}
	if data, _ := os.ReadFile(path); string(data) != skillBody {
		t.Error("stale skill was not replaced")
	}
}

func TestRemoveSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".claude", "skills", "dbgorilla")

	res, err := RemoveSkill(ScopeUser)
	if err != nil {
		t.Fatalf("removing what was never installed: %v", err)
	}
	if !res.Absent {
		t.Errorf("= %+v, want Absent when nothing is installed", res)
	}

	if _, err := InstallSkill(ScopeUser); err != nil {
		t.Fatal(err)
	}
	if res, err = RemoveSkill(ScopeUser); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if res.Absent {
		t.Error("reported nothing to remove when the skill was installed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists after removal", dir)
	}
	// Removal must not take the user's other skills with it.
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills")); err != nil {
		t.Errorf("the skills directory itself was removed: %v", err)
	}
}
