// Package offloadtest runs a throwaway MinIO for the uploads offload's
// Docker tests (WPGENIE_TEST_DOCKER=1): a bucket whose prefix is publicly
// readable, like the storage a site offloads to.
package offloadtest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Images used by the tests (MinIO no longer publishes to Docker Hub).
const (
	MinIOImage  = "chainguard/minio:latest"
	ClientImage = "chainguard/minio-client:latest"
)

// MinIO is a running MinIO on a Docker network of its own.
type MinIO struct {
	Network   string // containers on it reach MinIO at Endpoint
	Name      string
	Endpoint  string // http://<name>:9000, from the network
	HostURL   string // http://127.0.0.1:<port>, from the test process
	Bucket    string
	AccessKey string
	SecretKey string
}

func random() string {
	b := make([]byte, 5)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Start runs MinIO with a bucket; prefix (e.g. "site1/uploads/") is made
// anonymously readable. Everything is removed when the test ends. With
// E2E_NET set (the test runs in a container on that network, see make
// test-e2e), MinIO joins it and the test reaches it by name.
func Start(t *testing.T, prefix string) *MinIO {
	t.Helper()
	id := random()
	m := &MinIO{Network: os.Getenv("E2E_NET"), Name: "wpgtest-minio-" + id, Bucket: "media",
		AccessKey: "wpgtest" + id, SecretKey: "secret-" + id + "-" + random()}
	m.Endpoint = "http://" + m.Name + ":9000"
	if m.Network == "" {
		m.Network = "wpgtest-offload-" + id
		if out, err := exec.Command("docker", "network", "create", m.Network).CombinedOutput(); err != nil {
			t.Fatalf("docker network create: %v\n%s", err, out)
		}
		t.Cleanup(func() { exec.Command("docker", "network", "rm", m.Network).Run() })
	}
	out, err := exec.Command("docker", "run", "-d", "--name", m.Name, "--network", m.Network, "-p", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER="+m.AccessKey, "-e", "MINIO_ROOT_PASSWORD="+m.SecretKey,
		MinIOImage, "server", "/tmp/data").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run minio: %v\n%s", err, out)
	}
	// Registered after the network's cleanup: runs first.
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", m.Name).Run() })
	deadline := time.Now().Add(60 * time.Second)
	for {
		if os.Getenv("E2E_NET") != "" {
			m.HostURL = m.Endpoint
		} else if a, err := exec.Command("docker", "port", m.Name, "9000/tcp").Output(); err == nil && len(a) > 0 {
			m.HostURL = "http://" + strings.TrimSpace(strings.SplitN(string(a), "\n", 2)[0])
		}
		if m.HostURL != "" {
			if resp, err := http.Get(m.HostURL + "/minio/health/ready"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", m.Name).CombinedOutput()
			t.Fatalf("minio did not come up:\n%s", logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
	m.MC(t, "", "mb", "m/"+m.Bucket)
	m.MC(t, "", "anonymous", "set", "download", "m/"+m.Bucket+"/"+prefix)
	return m
}

// MC runs the MinIO client (alias "m" is this server) with stdin.
func (m *MinIO) MC(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"run", "--rm", "-i", "--network", m.Network,
		"-e", "MC_HOST_m=http://" + m.AccessKey + ":" + m.SecretKey + "@" + m.Name + ":9000", ClientImage}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mc %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// Put stores an object (key relative to the bucket).
func (m *MinIO) Put(t *testing.T, key, data string) {
	t.Helper()
	m.MC(t, data, "pipe", "m/"+m.Bucket+"/"+key)
}

// Get fetches an object anonymously from the test process.
func (m *MinIO) Get(t *testing.T, key string) (int, string) {
	t.Helper()
	resp, err := http.Get(m.HostURL + "/" + m.Bucket + "/" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

// RunAs is the user rclone's containers should run as in a test: the test's
// own when it isn't root (CI), so rclone can use its temporary directories;
// as root without capabilities it can't enter another user's 0700 ones.
func RunAs() string {
	if os.Geteuid() == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
}
