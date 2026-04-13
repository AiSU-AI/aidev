package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectTestCommandGoMod(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, label, err := DetectTestCommand(dir)
	if err != nil {
		t.Fatal(err)
	}
	if label != "go" {
		t.Errorf("label = %q, want go", label)
	}
	if len(cmd) != 3 || cmd[0] != "go" || cmd[1] != "test" {
		t.Errorf("cmd = %v, want [go test ./...]", cmd)
	}
}

func TestDetectTestCommandCargo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, label, err := DetectTestCommand(dir)
	if err != nil {
		t.Fatal(err)
	}
	if label != "cargo" || cmd[0] != "cargo" {
		t.Errorf("cmd = %v, label = %q", cmd, label)
	}
}

func TestDetectTestCommandPython(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, label, _ := DetectTestCommand(dir)
	if label != "python" || cmd[0] != "pytest" {
		t.Errorf("cmd = %v, label = %q", cmd, label)
	}
}

func TestDetectTestCommandNpm(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, label, _ := DetectTestCommand(dir)
	if label != "node" || cmd[0] != "npm" {
		t.Errorf("cmd = %v, label = %q", cmd, label)
	}
}

func TestDetectTestCommandGoTakesPriorityOverMakefile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\techo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, label, _ := DetectTestCommand(dir)
	if label != "go" {
		t.Errorf("label = %q, want go (priority should pick go over Makefile)", label)
	}
	_ = cmd
}

func TestDetectTestCommandMakefileOnlyWithTestTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("build:\n\techo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := DetectTestCommand(dir)
	if err == nil {
		t.Error("Makefile with no test target should NOT be detected as runnable")
	}
}

func TestDetectTestCommandMakefileWithTestTargetIsDetected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("build: ; echo build\ntest:\n\techo ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, label, err := DetectTestCommand(dir)
	if err != nil {
		t.Fatal(err)
	}
	if label != "make" || cmd[0] != "make" {
		t.Errorf("cmd = %v, label = %q", cmd, label)
	}
}

func TestDetectTestCommandErrorsOnEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	_, _, err := DetectTestCommand(dir)
	if err == nil {
		t.Error("empty repo should return an error")
	}
}

func TestTailStringShorterThanLimit(t *testing.T) {
	if got := tailString("hello", 100); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestTailStringLongerThanLimit(t *testing.T) {
	long := strings.Repeat("a", 1000)
	got := tailString(long, 100)
	if len(got) > 200 {
		t.Errorf("tail too long: %d chars", len(got))
	}
	if !strings.Contains(got, "[truncated]") {
		t.Errorf("expected truncation marker, got %q", got)
	}
}

func TestMakefileHasTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Makefile")
	os.WriteFile(path, []byte("build: ; echo build\ntest:\n\techo t\ninstall:\n\techo i\n"), 0o644)
	if !makefileHasTarget(path, "test") {
		t.Error("should find test target")
	}
	if !makefileHasTarget(path, "install") {
		t.Error("should find install target")
	}
	if makefileHasTarget(path, "lint") {
		t.Error("should not find lint target")
	}
}

// v0.5b Docker sandbox tests.

func TestWrapInDockerReadOnly(t *testing.T) {
	got := wrapInDocker("golang:1.24", "/home/user/repo", false, []string{"go", "test", "./..."})
	want := []string{
		"run", "--rm",
		"-v", "/home/user/repo:/work:ro",
		"-w", "/work",
		"golang:1.24",
		"go", "test", "./...",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d args, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestWrapInDockerWritable(t *testing.T) {
	got := wrapInDocker("python:3.12", "/repo", true, []string{"pytest"})
	// The mount arg should NOT end in :ro when writable is true.
	found := false
	for _, a := range got {
		if a == "/repo:/work" {
			found = true
		}
		if a == "/repo:/work:ro" {
			t.Errorf("read-only mount leaked into writable mode: %v", got)
		}
	}
	if !found {
		t.Errorf("mount arg missing from writable invocation: %v", got)
	}
}

func TestWrapInDockerPreservesCommandArgs(t *testing.T) {
	cmd := []string{"npm", "test", "--", "--coverage"}
	got := wrapInDocker("node:20", "/r", false, cmd)
	// The last len(cmd) args should be the command verbatim.
	tail := got[len(got)-len(cmd):]
	for i, c := range cmd {
		if tail[i] != c {
			t.Errorf("tail[%d] = %q, want %q", i, tail[i], c)
		}
	}
}

func TestSetSandboxFlipsFlags(t *testing.T) {
	tester := &Tester{}
	tester.SetSandbox("golang:1.24", true)
	if !tester.Sandbox {
		t.Error("Sandbox should be true after SetSandbox")
	}
	if tester.Image != "golang:1.24" {
		t.Errorf("Image = %q", tester.Image)
	}
	if !tester.Writable {
		t.Error("Writable should be true")
	}
}

func TestRunFailsFastWhenSandboxEnabledButNoImage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644)
	tester := &Tester{Sandbox: true} // Image intentionally empty
	_, err := tester.Run(nil, dir)
	if err == nil {
		t.Error("expected error when sandbox is enabled but no image is set")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error should mention image, got: %v", err)
	}
}
