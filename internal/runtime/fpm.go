package runtime

// PHP-FPM worker sizing. Every worker is a full PHP process running
// WordPress, so pm.max_children is what actually bounds a container's memory
// use: set it too high and a traffic spike gets the container OOM-killed by
// its --memory limit; too low and requests queue while memory sits idle.
//
// It also bounds database load: each busy worker holds one MariaDB
// connection, and every site shares one MariaDB (max_connections = 300).
const (
	// MinMaxChildren: even the smallest site must serve a slow request (a
	// long checkout, an admin save) without blocking every other visitor.
	MinMaxChildren = 2
	// MaxMaxChildren caps one replica's DB connections; beyond this, scale
	// out with more replicas (each has its own cap) rather than up.
	MaxMaxChildren = 32
)

// FPMMaxChildren returns pm.max_children for a replica with memoryMB of RAM.
//
// Things to weigh:
//   - Per-worker RSS: ~40 MB for a lean blog, 80–150 MB with WooCommerce or
//     a page builder. php.ini's memory_limit (256M) is the worst case for a
//     single request, not the typical one.
//   - Fixed overhead not available to workers: the FPM master process and
//     OPcache's shared memory (opcache.memory_consumption = 128 in php.ini).
//   - The result must stay within [MinMaxChildren, MaxMaxChildren], and must
//     never decrease when memoryMB grows (TestFPMMaxChildren checks both).
func FPMMaxChildren(memoryMB int) int {
	// TODO(you): replace this placeholder with a real sizing rule.
	return MinMaxChildren
}
