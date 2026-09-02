package ide

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// SkillName is the directory the skill is installed under, and the name
// declared in its front matter. Kept identical to MCPServerName so a user
// sees one word for one integration.
const SkillName = MCPServerName

// skillBody is the skill itself, embedded so the shipped file is reviewable
// as markdown rather than as a Go string literal.
//
//go:embed skill/SKILL.md
var skillBody string

// SkillResult describes what InstallSkill or RemoveSkill did, so the caller
// can say something true rather than something generic.
type SkillResult struct {
	Path    string
	Created bool // the skill was not there before
	Updated bool // an older copy was replaced
	NoOp    bool // what was already there is what we would have written
	Absent  bool // nothing to remove
}

// SkillDir returns the directory the skill lives in for a scope. User scope
// puts it in the home directory, where it applies to every project; project
// scope puts it beside the code, where it can be committed and shared.
func SkillDir(scope Scope) (string, error) {
	if scope == ScopeProject {
		cwd, err := getCWD()
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, ".claude", "skills", SkillName), nil
	}
	home := homeDir()
	if home == "" {
		return "", fmt.Errorf("cannot resolve home directory")
	}
	return filepath.Join(home, ".claude", "skills", SkillName), nil
}

// InstallSkill writes the skill for the given scope.
//
// Idempotent by content, not by existence: re-running rewrites the file only
// when this build ships something different, so a user who runs setup-ide
// habitually gets the current text without a write, a backup, or a line of
// output every time. There is no merge to do -- the directory belongs to this
// integration and holds one file -- so an out-of-date copy is replaced whole.
func InstallSkill(scope Scope) (SkillResult, error) {
	dir, err := SkillDir(scope)
	if err != nil {
		return SkillResult{}, err
	}
	path := filepath.Join(dir, "SKILL.md")
	res := SkillResult{Path: path}

	switch existing, readErr := os.ReadFile(path); {
	case readErr == nil && string(existing) == skillBody:
		res.NoOp = true
		return res, nil
	case readErr == nil:
		res.Updated = true
	case os.IsNotExist(readErr):
		res.Created = true
	default:
		return res, fmt.Errorf("cannot read existing skill at %s: %w", path, readErr)
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return res, fmt.Errorf("cannot create %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(skillBody), 0644); err != nil {
		return res, fmt.Errorf("cannot write skill to %s: %w", path, err)
	}
	return res, nil
}

// RemoveSkill deletes the skill directory for the given scope.
//
// Removing the directory rather than the one file inside it is safe because
// nothing else may live there: it is named for this integration, it is created
// by InstallSkill, and a user with their own instructions to add would put
// them in a skill of their own name.
func RemoveSkill(scope Scope) (SkillResult, error) {
	dir, err := SkillDir(scope)
	if err != nil {
		return SkillResult{}, err
	}
	res := SkillResult{Path: filepath.Join(dir, "SKILL.md")}
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		res.Absent = true
		return res, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return res, fmt.Errorf("cannot remove %s: %w", dir, err)
	}
	return res, nil
}
