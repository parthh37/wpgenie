package site

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRestoreScriptInRealImage runs the snapshot tar and restoreScript in
// the PHP image, as the site user, on a real docroot. Needs Docker and a
// built wpgenie/php:8.3 (make php-image).
func TestRestoreScriptInRealImage(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1")
	}
	if exec.Command("docker", "image", "inspect", "wpgenie/php:8.3").Run() != nil {
		t.Skip("wpgenie/php:8.3 not built")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	root := filepath.Join(dir, "public")
	write := func(name, body string) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o777)
		if err := os.WriteFile(p, []byte(body), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"index.php": "v1", ".htaccess": "rules", "wp-includes/version.php": "6.8",
		"wp-content/plugins/akismet/akismet.php": "5.3", "wp-content/uploads/2026/photo.jpg": "img",
		"wp-content/cache/wpgenie/index.html": "cached",
	} {
		write(name, body)
	}
	sh := func(stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("docker", append([]string{"run", "--rm", "-i", "--user", "82:82", "--entrypoint", "",
			"-v", dir + ":" + dir, "wpgenie/php:8.3"}, args...)...)
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		if err != nil {
			err = &exec.ExitError{Stderr: errb.Bytes()}
		}
		return out.Bytes(), err
	}
	snap, err := sh(nil, append(snapshotArgs, root, ".")...)
	if err != nil {
		t.Fatalf("snapshot: %v %s", err, err.(*exec.ExitError).Stderr)
	}

	// The "update": changed, added and removed files, plus new uploads.
	write("index.php", "v2")
	write("wp-content/plugins/akismet/akismet.php", "5.7")
	write("wp-content/plugins/akismet/new-in-5.7.php", "x")
	write("added-by-update.php", "x")
	write("wp-content/uploads/2026/during-update.jpg", "new upload")
	os.Remove(filepath.Join(root, "wp-includes/version.php"))

	// A cut stream must leave the site exactly as it is.
	if _, err := sh(snap[:len(snap)/2], "sh", "-c", restoreScript, "sh", root); err == nil {
		t.Fatal("a truncated snapshot restored without error")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "index.php")); string(b) != "v2" {
		t.Fatal("a failed restore touched the live site")
	}

	if _, err := sh(snap, "sh", "-c", restoreScript, "sh", root); err != nil {
		t.Fatalf("restore: %v %s", err, err.(*exec.ExitError).Stderr)
	}
	for name, want := range map[string]string{
		"index.php": "v1", ".htaccess": "rules", "wp-includes/version.php": "6.8",
		"wp-content/plugins/akismet/akismet.php": "5.3",
		"wp-content/uploads/2026/photo.jpg":      "img", "wp-content/uploads/2026/during-update.jpg": "new upload",
		"wp-content/cache/wpgenie/index.html": "cached",
	} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	for _, gone := range []string{"added-by-update.php", "wp-content/plugins/akismet/new-in-5.7.php", ".wpgenie-restore"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the restore", gone)
		}
	}
}
