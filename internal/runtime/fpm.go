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

const (
	// fixedOverheadMB is memory no worker can use: OPcache's shared segment
	// (opcache.memory_consumption = 128 in php.ini, interned strings
	// included) plus the FPM master and the container's own processes.
	fixedOverheadMB = 160
	// workerBudgetMB is the average RSS planned per worker. A lean blog
	// worker sits near 40 MB and WooCommerce/page builders reach 100+ MB;
	// pm.max_requests = 500 recycles workers before leaks accumulate, and
	// ondemand workers rarely all peak at once. php.ini's memory_limit (256M)
	// is a per-request ceiling, not a planning figure: sizing by it would
	// give a 1 GB replica 3 workers and leave its CPU idle under load.
	workerBudgetMB = 64
)

// FPMMaxChildren returns pm.max_children for a replica with memoryMB of RAM:
// what is left after the fixed overhead, divided by the per-worker budget,
// clamped to [MinMaxChildren, MaxMaxChildren]. 512 MB → 5, 1 GB → 13,
// 2 GB → 29 workers.
func FPMMaxChildren(memoryMB int) int {
	n := (memoryMB - fixedOverheadMB) / workerBudgetMB
	return min(max(n, MinMaxChildren), MaxMaxChildren)
}
