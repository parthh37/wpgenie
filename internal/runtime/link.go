package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Links runs link containers: on a node running replicas of a site that
// lives on another server, the replicas reach that server's MariaDB and
// Valkey through wpg-link-<home>, the wpgenie binary in "link" mode tunnelling
// them over the cluster's mutual TLS (see site/spread.go). The container
// sees nothing but the binary and this node's cluster identity, read-only;
// it runs with no capabilities and a read-only filesystem.
type Links struct {
	Docker     *Docker
	Image      string // any image with a filesystem; the binary is static
	Network    string
	ClusterDir string // the node's cluster identity
	Binary     string // this wpgenie binary
}

// spec changes whenever the link must be re-created: another home address,
// or a renewed node certificate (the link reads it only when it starts).
func (l *Links) spec(home, address string) string {
	cert, _ := os.ReadFile(filepath.Join(l.ClusterDir, "node.pem"))
	sum := sha256.Sum256([]byte(strings.Join([]string{home, address, l.Image, l.Network, l.ClusterDir, l.Binary, string(cert)}, "|")))
	return hex.EncodeToString(sum[:6])
}

// Ensure runs the link for home at address, re-creating it if anything
// about it changed.
func (l *Links) Ensure(ctx context.Context, name, home, address string) error {
	spec := l.spec(home, address)
	out, err := l.Docker.Run(ctx, nil, "inspect", "-f", `{{index .Config.Labels "wpgenie.link-spec"}} {{.State.Running}}`, name)
	if err == nil && strings.TrimSpace(string(out)) == spec+" true" {
		return nil
	}
	l.Docker.Run(ctx, nil, "rm", "-f", name)
	_, err = l.Docker.Run(ctx, nil, "run", "-d",
		"--name", name,
		"--label", "wpgenie.link="+home,
		"--label", "wpgenie.link-spec="+spec,
		"--restart", "unless-stopped",
		"--network", l.Network,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--memory", "64m",
		"--pids-limit", "64",
		// Root inside, to read the node's key (root 0600); no capabilities.
		"--user", "0:0",
		"-v", l.Binary+":/wpgenie:ro",
		// This node's identity only (the files, not the directory).
		"-v", filepath.Join(l.ClusterDir, "node.key")+":/cluster/node.key:ro",
		"-v", filepath.Join(l.ClusterDir, "node.pem")+":/cluster/node.pem:ro",
		"-v", filepath.Join(l.ClusterDir, "ca.pem")+":/cluster/ca.pem:ro",
		"-v", filepath.Join(l.ClusterDir, "node.json")+":/cluster/node.json:ro",
		"--entrypoint", "/wpgenie",
		l.Image,
		"link", "--dir", "/cluster", "--node", home, "--address", address)
	if err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return nil
}

// Remove stops and deletes a link container.
func (l *Links) Remove(ctx context.Context, name string) error {
	_, err := l.Docker.Run(ctx, nil, "rm", "-f", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}
