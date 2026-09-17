package memguard

import (
	"path/filepath"
	"testing"
)

const userMem = "---\nname: who\ndescription: about the person\nmetadata:\n  type: user\n---\n\nPaul prefers X.\n"
const projMem = "---\nname: lesson\ndescription: a team lesson\nmetadata:\n  type: project\n---\n\nThe suite needs -p 1.\n"

func TestCheckFileWrite(t *testing.T) {
	const dir = "/home/me/.claude/projects/-home-me-proj/memory/"
	for _, c := range []struct {
		name, path, content string
		deny                bool
	}{
		{name: "project-type note denied", path: dir + "lesson.md", content: projMem, deny: true},
		{name: "user-type note allowed", path: dir + "who.md", content: userMem},
		{name: "MEMORY.md allowed", path: dir + "MEMORY.md", content: "# Memory index\n"},
		// An Edit passes the file already on disk; the classification is the same.
		{name: "edit of existing project note denied", path: dir + "old.md", content: projMem, deny: true},
		{name: "edit of existing user note allowed", path: dir + "old.md", content: userMem},
		// Unreadable content falls back to the path, which is a memory note.
		{name: "unknown content denied", path: dir + "x.md", content: "", deny: true},
		// A worktree spells the project segment differently and must still match.
		{name: "worktree project dir denied", path: "/home/me/.claude/projects/-home-me-proj-worker/memory/x.md", content: projMem, deny: true},
		// Anything outside the directory is none of this guard's business.
		{name: "repo file allowed", path: "/home/me/proj/internal/store/x.go", content: projMem},
		{name: "similarly named dir allowed", path: "/home/me/proj/memory/x.md", content: projMem},
		{name: "empty path allowed", path: "", content: projMem},
		// `type: user` in the BODY is prose, not a declaration.
		{name: "type user in body denied", path: dir + "x.md", content: "---\nname: n\n---\n\ntype: user\n", deny: true},
	} {
		if got := CheckFileWrite(c.path, c.content); got.Deny != c.deny {
			t.Errorf("%s: CheckFileWrite(%q) deny=%v want %v", c.name, c.path, got.Deny, c.deny)
		}
	}
}

// The hosts pass a HOST path, not a slash path. On Windows that is spelled with
// backslashes and a drive letter, which matched none of the forward-slash-only
// rules: #211 shipped with the guard failing OPEN there, allowing every private
// memory write while reporting nothing.
//
// The portability note is the point of the split below, not pedantry.
// filepath.ToSlash only rewrites os.PathSeparator, so it is a NO-OP on Linux
// and macOS. A hardcoded backslash string is therefore NOT a cross-platform
// row: on unix a backslash is an ordinary filename character, such a path is
// not a memory path at all, and an ungated `deny: true` row would fail there.
// So the portable rows build the separator with FromSlash, and only the literal
// drive-letter spelling is gated.
func TestCheckFileWrite_HostPathSpelling(t *testing.T) {
	// FromSlash yields backslashes on Windows and changes nothing on unix.
	// Either way it is the spelling THIS host's agent actually sends.
	native := filepath.FromSlash("/home/me/.claude/projects/-home-me-proj/memory/lesson.md")
	if got := CheckFileWrite(native, projMem); !got.Deny {
		t.Errorf("a host-spelled memory note must be denied, got allow for %q", native)
	}
	nativeIndex := filepath.FromSlash("/home/me/.claude/projects/-home-me-proj/memory/MEMORY.md")
	if got := CheckFileWrite(nativeIndex, "# Memory index\n"); got.Deny {
		t.Errorf("MEMORY.md must stay exempt when host-spelled, got deny for %q", nativeIndex)
	}

	if filepath.Separator == '/' {
		return // the rows below are a Windows spelling, meaningless elsewhere
	}
	// The real spelling from a Windows session, drive letter and all. The
	// MEMORY.md row is not redundant: it pins the second defect, path.Base,
	// which returns the WHOLE string for a backslash path and so killed the
	// exemption. That one stayed masked while the regex bailed out first.
	for _, c := range []struct {
		name, path, content string
		deny                bool
	}{
		{name: "drive-letter note denied", path: `C:\Users\me\.claude\projects\-c-users-me-proj\memory\lesson.md`, content: projMem, deny: true},
		{name: "drive-letter MEMORY.md allowed", path: `C:\Users\me\.claude\projects\-c-users-me-proj\memory\MEMORY.md`, content: "# Memory index\n"},
		{name: "drive-letter user note allowed", path: `C:\Users\me\.claude\projects\-c-users-me-proj\memory\who.md`, content: userMem},
		{name: "drive-letter repo file allowed", path: `C:\Users\me\proj\internal\store\x.go`, content: projMem},
	} {
		if got := CheckFileWrite(c.path, c.content); got.Deny != c.deny {
			t.Errorf("%s: CheckFileWrite(%q) deny=%v want %v", c.name, c.path, got.Deny, c.deny)
		}
	}
}

func TestCheckBash(t *testing.T) {
	const d = "~/.claude/projects/-home-me-proj/memory"
	for _, c := range []struct {
		name, cmd string
		deny      bool
	}{
		// Writes.
		{name: "redirect", cmd: "echo hi > " + d + "/x.md", deny: true},
		{name: "append to MEMORY.md", cmd: "printf '%s\\n' line >> " + d + "/MEMORY.md", deny: true},
		{name: "tee", cmd: "echo hi | tee " + d + "/x.md", deny: true},
		{name: "sed -i", cmd: "sed -i 's/a/b/' " + d + "/x.md", deny: true},
		{name: "rm", cmd: "rm " + d + "/x.md", deny: true},
		{name: "mv", cmd: "mv a.md " + d + "/x.md", deny: true},
		{name: "cp", cmd: "cp a.md " + d + "/x.md", deny: true},
		{name: "dd", cmd: "dd if=a.md of=" + d + "/x.md", deny: true},
		{name: "install", cmd: "install a.md " + d + "/x.md", deny: true},
		{name: "heredoc", cmd: "cat > " + d + "/x.md <<'EOF'\nhi\nEOF", deny: true},
		{name: "subshell redirect", cmd: "(echo hi) > " + d + "/x.md", deny: true},
		{name: "python -c", cmd: "python3 -c \"open('" + d + "/x.md','w').write('hi')\"", deny: true},
		{name: "perl -e", cmd: "perl -e 'open(F,\">" + d + "/x.md\")'", deny: true},
		// Reads — the case the word-matching prototype got wrong.
		{name: "cat read", cmd: "cat " + d + "/MEMORY.md"},
		{name: "sed -n read", cmd: "sed -n '1,5p' " + d + "/x.md"},
		{name: "echo of a read", cmd: "echo $(cat " + d + "/MEMORY.md)"},
		{name: "ls", cmd: "ls " + d},
		{name: "grep", cmd: "grep -r knomit " + d},
		// Writes elsewhere are none of this guard's business.
		{name: "write outside", cmd: "echo hi > /home/me/proj/notes.md"},
		{name: "empty", cmd: ""},
	} {
		if got := CheckBash(c.cmd); got.Deny != c.deny {
			t.Errorf("%s: CheckBash(%q) deny=%v want %v", c.name, c.cmd, got.Deny, c.deny)
		}
	}
}
