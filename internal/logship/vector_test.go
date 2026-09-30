package logship

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func goldenInputs() map[string]vectorInput {
	full := DefaultSettings()
	full.Enabled = true
	full.Destination = testDestination()
	full.Types[TypeContainers] = true

	small := DefaultSettings()
	small.Enabled = true
	small.Destination = Destination{Provider: "r2", Endpoint: "https://0123456789abcdef.r2.cloudflarestorage.com",
		Bucket: "archive", Prefix: "", AccessKeyID: "0123456789abcdef0123456789abcdef", SecretKey: "R2SecretValue9"}
	small.Compression, small.BatchMaxMB, small.BatchMaxSeconds = "zstd", 50, 900

	return map[string]vectorInput{
		"full":  {Settings: full, Server: "panel", Tailed: []string{TypeAccess, TypeMail}, AccessLog: "access.log"},
		"spool": {Settings: small, Server: "web-2"},
	}
}

// The generated configuration, compared with testdata/<name>.golden.yaml
// (go test -run TestVectorConfigGolden -update rewrites them).
func TestVectorConfigGolden(t *testing.T) {
	for name, in := range goldenInputs() {
		got, err := vectorConfig(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		path := filepath.Join("testdata", name+".golden.yaml")
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: the configuration changed (go test -run TestVectorConfigGolden -update, then review the diff):\n%s", name, got)
		}
		// Never the keys: they're in the credentials file.
		if bytes.Contains(got, []byte(in.Settings.Destination.SecretKey)) ||
			bytes.Contains(got, []byte(in.Settings.Destination.AccessKeyID)) || bytes.Contains(got, []byte("secret_access_key")) {
			t.Errorf("%s: the configuration holds the secret", name)
		}
	}
}

func TestVectorConfigRefuses(t *testing.T) {
	in := goldenInputs()["full"]
	for name, change := range map[string]func(*vectorInput){
		"server with a slash":     func(in *vectorInput) { in.Server = "a/b" },
		"server with a template":  func(in *vectorInput) { in.Server = "{{ x }}" },
		"no secret":               func(in *vectorInput) { in.Settings.Destination.SecretKey = "" },
		"access log name":         func(in *vectorInput) { in.AccessLog = "../x.log" },
		"unknown tailed log type": func(in *vectorInput) { in.Tailed = []string{"kernel"} },
	} {
		c := in
		c.Settings.Destination = in.Settings.Destination
		change(&c)
		if _, err := vectorConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Strings can't break out of their quotes, whatever they hold.
func TestYAMLQuoting(t *testing.T) {
	out, err := marshalYAML(ymap{{"a", "x\"\n  evil: true\\"}, {"b", []string{"p: q", "#c"}}, {"c", yblock("line 1\n\nline 3\n")}})
	if err != nil {
		t.Fatal(err)
	}
	want := "a: \"x\\\"\\n  evil: true\\\\\"\nb: [\"p: q\", \"#c\"]\nc: |\n  line 1\n\n  line 3\n"
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	if _, err := marshalYAML(ymap{{"bad key", 1}}); err == nil {
		t.Error("unsafe key accepted")
	}
}

// Vector itself validates the generated configurations (Docker, with
// WPGENIE_TEST_DOCKER=1: it pulls the image).
func TestVectorValidate(t *testing.T) {
	if os.Getenv("WPGENIE_TEST_DOCKER") != "1" {
		t.Skip("set WPGENIE_TEST_DOCKER=1 to validate with Vector in Docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker isn't running")
	}
	image := "timberio/vector:0.58.0-alpine@sha256:5dcf67db0ee378caa87f3395cb9484ebe3e97bb0334d119f2ac33116e00c5773"
	for name, in := range goldenInputs() {
		cfg, err := vectorConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "tables"), 0o755)
		os.WriteFile(filepath.Join(dir, "vector.yaml"), cfg, 0o644)
		os.WriteFile(filepath.Join(dir, "tables", "sites.csv"), []byte("host,site\nexample.com,s1\n"), 0o644)
		os.MkdirAll(filepath.Join(dir, "secrets"), 0o755)
		creds, _ := credentialsFile(in.Settings.Destination)
		os.WriteFile(filepath.Join(dir, "secrets", "credentials"), creds, 0o644)
		out, err := exec.Command("docker", "run", "--rm",
			"-v", filepath.Join(dir, "vector.yaml")+":"+ctrConfigFile+":ro",
			"-v", filepath.Join(dir, "tables")+":"+ctrTables+":ro",
			"-v", filepath.Join(dir, "secrets")+":"+ctrSecrets+":ro",
			image, "validate", "--no-environment", ctrConfigFile).CombinedOutput()
		if err != nil {
			t.Errorf("%s: vector validate: %v\n%s", name, err, out)
		}
	}
}
