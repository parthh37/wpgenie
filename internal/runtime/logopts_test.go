package runtime

import (
	"slices"
	"testing"
)

func TestContainerLogLimit(t *testing.T) {
	t.Cleanup(func() { SetContainerLogLimit(0, 0) })
	run := []string{"run", "-d", "--name", "x", "img"}
	if got := withLogOpts(run); !slices.Equal(got, run) {
		t.Errorf("no limit set: %q", got)
	}
	SetContainerLogLimit(10, 3)
	if mb, n := ContainerLogLimit(); mb != 10 || n != 3 {
		t.Errorf("ContainerLogLimit = %d, %d", mb, n)
	}
	want := []string{"run", "--log-driver", "json-file", "--log-opt", "max-size=10m", "--log-opt", "max-file=3",
		"-d", "--name", "x", "img"}
	if got := withLogOpts(run); !slices.Equal(got, want) {
		t.Errorf("detached run: %q", got)
	}
	for _, args := range [][]string{
		{"run", "--rm", "-i", "img"},                 // throwaway: its logs go with it
		{"run", "-d", "--log-driver", "none", "img"}, // chooses its own
		{"exec", "-d", "x", "true"},                  // not a new container
		{"inspect", "--", "-d"},                      // not a run at all
	} {
		if got := withLogOpts(args); !slices.Equal(got, args) {
			t.Errorf("%q became %q", args, got)
		}
	}
	SetContainerLogLimit(0, 0)
	if got := withLogOpts(run); !slices.Equal(got, run) {
		t.Errorf("limit removed: %q", got)
	}
	if mb, _ := ContainerLogLimit(); mb != 0 {
		t.Errorf("ContainerLogLimit after removal = %d", mb)
	}
}
