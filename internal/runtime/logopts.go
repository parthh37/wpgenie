package runtime

import (
	"slices"
	"strconv"
	"sync/atomic"
)

// Docker keeps a container's stdout and stderr in a JSON file that grows
// without limit by default. When logs are shipped elsewhere (internal/
// logship), the containers this daemon starts get size-capped, rotated
// logs instead: every `docker run -d` any Docker of this process runs
// (sites' PHP, the mail server, SFTP, phpMyAdmin, tunnels, the shipper)
// carries the options. Containers keep what they were started with until
// they're recreated; the options don't count in their spec hashes, so
// changing them never restarts anything by itself.

var logOpts atomic.Pointer[[]string]

// SetContainerLogLimit caps the json-file logs of the containers started
// from now on at files rotated files of maxSizeMB each; maxSizeMB <= 0
// removes the cap (Docker's defaults). Only for Docker's default json-file
// driver: set it only when that is the daemon's logging driver.
func SetContainerLogLimit(maxSizeMB, files int) {
	if maxSizeMB <= 0 {
		logOpts.Store(nil)
		return
	}
	files = max(files, 1)
	opts := []string{"--log-driver", "json-file",
		"--log-opt", "max-size=" + strconv.Itoa(maxSizeMB) + "m", "--log-opt", "max-file=" + strconv.Itoa(files)}
	logOpts.Store(&opts)
}

// ContainerLogLimit reports the cap set by SetContainerLogLimit (0: none).
func ContainerLogLimit() (maxSizeMB, files int) {
	p := logOpts.Load()
	if p == nil {
		return 0, 0
	}
	o := *p
	maxSizeMB, _ = strconv.Atoi(o[3][len("max-size=") : len(o[3])-1])
	files, _ = strconv.Atoi(o[5][len("max-file="):])
	return maxSizeMB, files
}

// withLogOpts adds the log options to a detached `docker run` that doesn't
// choose its own logging.
func withLogOpts(args []string) []string {
	p := logOpts.Load()
	if p == nil || len(args) < 2 || args[0] != "run" {
		return args
	}
	detached := false
	for _, a := range args[1:] {
		switch a {
		case "--log-driver", "--log-opt":
			return args
		case "-d", "--detach":
			detached = true
		}
	}
	if !detached {
		return args // throwaway containers (--rm): their logs go with them
	}
	return slices.Concat(args[:1], *p, args[1:])
}
