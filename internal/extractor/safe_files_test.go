package extractor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootedFileAccessRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "report.csv")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readRootedFile(link); err == nil {
		t.Fatal("rooted read followed a symlink outside the selected directory")
	}
	if err := removeRootedFile(link); err != nil {
		t.Fatalf("rooted removal should remove the link itself: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "secret" {
		t.Fatalf("outside file changed: data=%q err=%v", data, err)
	}
}

func TestCreateExclusiveRootedFileDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")
	file, closeRoot, err := createExclusiveRootedFile(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("first"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	closeRoot()
	if _, closeAgain, err := createExclusiveRootedFile(path, 0o600); err == nil {
		closeAgain()
		t.Fatal("exclusive rooted file creation overwrote an existing artifact")
	}
}
