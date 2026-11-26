package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A deploy directory that older versions filled with a git clone ends up with
// only what the app itself wrote: tracked files, the folders that held them
// and .git go; untracked data stays, even inside a folder the repo also used.
func TestRemoveOldCheckout_LeavesOnlyAppData(t *testing.T) {
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	for _, f := range []string{"docker-compose.yml", "src/components/App.tsx", "src/assets/logo.svg", "public/index.html", ".github/workflows/deploy.yml", ".mountabo/deploy.sh", "dir with space/file.txt"} {
		write(f, "tracked")
	}
	run("git", "init", "-q")
	run("git", "add", "-A")
	run("git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	write("data/uploads/photo.jpg", "app data")
	write("public/user-upload.png", "app data in a repo folder")

	cmd := exec.Command("bash", "-c", removeOldCheckout)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup failed: %v\n%s", err, out)
	}

	var left []string
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
		if p != dir {
			rel, _ := filepath.Rel(dir, p)
			left = append(left, rel)
		}
		return nil
	})
	want := map[string]bool{"data": true, "data/uploads": true, "data/uploads/photo.jpg": true, "public": true, "public/user-upload.png": true}
	for _, p := range left {
		if !want[p] {
			t.Errorf("left behind: %s", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("removed app data: %s", p)
	}
}
