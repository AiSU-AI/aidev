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
