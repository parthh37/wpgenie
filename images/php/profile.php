<?php
/**
 * WPGenie plugin profiler.
 *
 * The daemon runs this with the PHP CLI inside a site's container, under the
 * same jail as web requests (open_basedir, no exec()), to render the front
 * page once and measure what each plugin, mu-plugin and the theme costs:
 *
 *   - load: time and memory to include a plugin's main file (plugin_loaded
 *     fires after each one, in order);
 *   - hooks: time spent in its hook callbacks, excluding callbacks of others
 *     that it triggers (callbacks are wrapped in place, keeping their keys,
 *     so remove_action() and has_action() still find them);
 *   - queries: database queries issued while its code was running.
 *
 * The rendered page is discarded; the report is printed after a marker line
 * as one line of JSON. It lives read-only in the image, like the page cache.
 *
 * No strict_types: callbacks are called from this file, and strict mode
 * would make WordPress's usual type juggling fail inside plugins.
 *
 * Usage: php profile.php <docroot>   (HTTP_HOST in the environment)
 */

if (PHP_SAPI !== 'cli' || $argc < 2) {
	fwrite(STDERR, "usage: php profile.php <docroot>\n");
	exit(2);
}
$wpg_root = rtrim($argv[1], '/');
if (!is_file($wpg_root . '/wp-blog-header.php')) {
	fwrite(STDERR, "no WordPress in $wpg_root\n");
	exit(2);
}
chdir($wpg_root);

$wpg_host = getenv('HTTP_HOST') ?: 'localhost';
$_SERVER = array_merge($_SERVER, [
	'HTTP_HOST' => $wpg_host,
	'SERVER_NAME' => $wpg_host,
	'SERVER_PORT' => '443',
	'HTTPS' => 'on',
	'REQUEST_METHOD' => 'GET',
	'REQUEST_URI' => '/',
	'QUERY_STRING' => '',
	'SCRIPT_NAME' => '/index.php',
	'PHP_SELF' => '/index.php',
	'SCRIPT_FILENAME' => $wpg_root . '/index.php',
	'SERVER_PROTOCOL' => 'HTTP/1.1',
	'REMOTE_ADDR' => '127.0.0.1',
	'HTTP_USER_AGENT' => 'WPGenie-Profiler',
	'HTTP_ACCEPT' => 'text/html',
]);
unset($_SERVER['argv'], $_SERVER['argc']);

define('WP_USE_THEMES', true);
define('DONOTCACHEPAGE', true); // never store this render in the page cache

final class WPGenie_Profiler
{
	public static $t0;
	/** @var array<int, array{0: string, 1: int, 2: int}> owner, start, time in children */
	public static $stack = [];
	public static $owners = []; // owner => stats
	public static $queue = [];  // active plugin files still to load
	public static $loading = null; // [owner, start, memory]
	public static $theme_start = null;
	public static $redirect = null;
	public static $output_bytes = 0;
	private static $wrappers = [];
	private static $file_owner = [];

	private static function &stats($owner)
	{
		if (!isset(self::$owners[$owner])) {
			self::$owners[$owner] = ['load_ns' => 0, 'load_bytes' => 0, 'hook_ns' => 0, 'calls' => 0, 'queries' => 0];
		}
		return self::$owners[$owner];
	}

	/** The plugin, mu-plugin or theme a file belongs to; null for core. */
	public static function owner_of_file($file)
	{
		if (isset(self::$file_owner[$file])) {
			return self::$file_owner[$file];
		}
		$owner = null;
		foreach ([
			'plugin' => defined('WP_PLUGIN_DIR') ? WP_PLUGIN_DIR : null,
			'mu-plugin' => defined('WPMU_PLUGIN_DIR') ? WPMU_PLUGIN_DIR : null,
			'theme' => defined('WP_CONTENT_DIR') ? WP_CONTENT_DIR . '/themes' : null,
		] as $kind => $dir) {
			if ($dir && strncmp($file, $dir . '/', strlen($dir) + 1) === 0) {
				$rel = substr($file, strlen($dir) + 1);
				$slug = strpos($rel, '/') !== false ? strstr($rel, '/', true) : basename($rel, '.php');
				$owner = $kind . ':' . $slug;
				break;
			}
		}
		return self::$file_owner[$file] = $owner;
	}

	private static function owner_of_callback($fn)
	{
		try {
			if ($fn instanceof Closure) {
				$r = new ReflectionFunction($fn);
			} elseif (is_string($fn)) {
				$r = strpos($fn, '::') !== false ? new ReflectionMethod($fn) : new ReflectionFunction($fn);
			} elseif (is_array($fn) && count($fn) === 2) {
				$r = new ReflectionMethod($fn[0], $fn[1]);
			} elseif (is_object($fn) && method_exists($fn, '__invoke')) {
				$r = new ReflectionMethod($fn, '__invoke');
			} else {
				return null;
			}
			$file = $r->getFileName();
			return $file ? self::owner_of_file($file) : null;
		} catch (Throwable $e) {
			return null; // __call magic and the like: left unattributed
		}
	}

	public static function enter($owner)
	{
		self::$stack[] = [$owner, hrtime(true), 0];
		$s = &self::stats($owner);
		$s['calls']++;
	}

	public static function leave()
	{
		$frame = array_pop(self::$stack);
		if ($frame === null) {
			return;
		}
		$elapsed = hrtime(true) - $frame[1];
		$s = &self::stats($frame[0]);
		$s['hook_ns'] += $elapsed - $frame[2];
		if (self::$stack) {
			self::$stack[count(self::$stack) - 1][2] += $elapsed;
		}
	}

	/** Wraps every not-yet-wrapped callback that belongs to a plugin or theme. */
	public static function wrap_all()
	{
		global $wp_filter;
		foreach ($wp_filter as $hook) {
			if (!($hook instanceof WP_Hook)) {
				continue;
			}
			foreach ($hook->callbacks as $priority => $callbacks) {
				foreach ($callbacks as $id => $cb) {
					$fn = $cb['function'];
					if ($fn instanceof Closure && isset(self::$wrappers[spl_object_id($fn)])) {
						continue;
					}
					$owner = self::owner_of_callback($fn);
					if ($owner === null) {
						continue;
					}
					$wrapper = static function (...$args) use ($fn, $owner) {
						WPGenie_Profiler::enter($owner);
						try {
							return $fn(...$args);
						} finally {
							WPGenie_Profiler::leave();
						}
					};
					self::$wrappers[spl_object_id($wrapper)] = $wrapper;
					$hook->callbacks[$priority][$id]['function'] = $wrapper;
				}
			}
		}
		return func_num_args() ? func_get_arg(0) : null; // also used as a filter
	}

	public static function current()
	{
		if (self::$stack) {
			return self::$stack[count(self::$stack) - 1][0];
		}
		return self::$loading ? self::$loading[0] : 'core';
	}

	public static function count_query($query)
	{
		$s = &self::stats(self::current());
		$s['queries']++;
		return $query;
	}

	private static function begin_load()
	{
		self::$loading = null;
		$file = array_shift(self::$queue);
		if ($file !== null && ($owner = self::owner_of_file($file)) !== null) {
			self::$loading = [$owner, hrtime(true), memory_get_usage()];
		}
	}

	public static function plugins_start()
	{
		self::$queue = function_exists('wp_get_active_and_valid_plugins') ? wp_get_active_and_valid_plugins() : [];
		self::begin_load();
	}

	public static function plugin_loaded($file)
	{
		if (self::$loading && self::$loading[0] === self::owner_of_file($file)) {
			$s = &self::stats(self::$loading[0]);
			$s['load_ns'] += hrtime(true) - self::$loading[1];
			$s['load_bytes'] += max(0, memory_get_usage() - self::$loading[2]);
		}
		self::begin_load();
	}

	public static function theme_start()
	{
		self::$theme_start = [hrtime(true), memory_get_usage()];
	}

	public static function theme_loaded()
	{
		if (self::$theme_start && function_exists('get_stylesheet')) {
			$s = &self::stats('theme:' . get_stylesheet());
			$s['load_ns'] += hrtime(true) - self::$theme_start[0];
			$s['load_bytes'] += max(0, memory_get_usage() - self::$theme_start[1]);
		}
		self::wrap_all();
	}

	public static function redirect($location, $status = 302)
	{
		self::$redirect = ['location' => (string) $location, 'status' => (int) $status];
		return $location;
	}

	/** Output buffer callback: the page is measured, never printed. */
	public static function discard($buffer)
	{
		self::$output_bytes += strlen($buffer);
		return '';
	}

	public static function report()
	{
		global $wpdb;
		// WordPress flushes every buffer on shutdown (wp_ob_end_flush_all);
		// ours discards what passes through it.
		while (ob_get_level() > 0) {
			ob_end_clean();
		}
		$owners = [];
		foreach (self::$owners as $owner => $s) {
			$owners[$owner] = [
				'load_ms' => round($s['load_ns'] / 1e6, 2),
				'load_kb' => intdiv($s['load_bytes'], 1024),
				'hook_ms' => round($s['hook_ns'] / 1e6, 2),
				'calls' => $s['calls'],
				'queries' => $s['queries'],
			];
		}
		$code = http_response_code();
		echo "\n@@WPGENIE-PROFILE@@\n", json_encode([
			'version' => 1,
			'total_ms' => round((hrtime(true) - self::$t0) / 1e6, 2),
			'peak_memory_kb' => intdiv(memory_get_peak_usage(), 1024),
			'queries' => isset($wpdb->num_queries) ? (int) $wpdb->num_queries : 0,
			'output_bytes' => self::$output_bytes,
			'status' => $code === false ? 0 : $code,
			'redirect' => self::$redirect,
			'owners' => (object) $owners,
		], JSON_UNESCAPED_SLASHES), "\n";
	}
}

WPGenie_Profiler::$t0 = hrtime(true);

// Hooks registered before WordPress loads are picked up by
// WP_Hook::build_preinitialized_hooks() when plugin.php loads.
$wpg_hook = static function ($callback, $priority = 10, $args = 1) {
	return [$priority => [['function' => $callback, 'accepted_args' => $args]]];
};
$wp_filter = [
	'muplugins_loaded' => $wpg_hook('WPGenie_Profiler::plugins_start', PHP_INT_MAX, 0),
	'plugin_loaded' => $wpg_hook('WPGenie_Profiler::plugin_loaded', PHP_INT_MIN),
	'plugins_loaded' => $wpg_hook('WPGenie_Profiler::wrap_all', PHP_INT_MIN, 0),
	'setup_theme' => $wpg_hook('WPGenie_Profiler::theme_start', PHP_INT_MAX, 0),
	'after_setup_theme' => $wpg_hook('WPGenie_Profiler::theme_loaded', PHP_INT_MIN, 0),
	'init' => $wpg_hook('WPGenie_Profiler::wrap_all', PHP_INT_MIN, 0),
	'wp_loaded' => $wpg_hook('WPGenie_Profiler::wrap_all', PHP_INT_MIN, 0),
	'wp' => $wpg_hook('WPGenie_Profiler::wrap_all', PHP_INT_MIN, 0),
	'template_redirect' => $wpg_hook('WPGenie_Profiler::wrap_all', PHP_INT_MIN, 0),
	'wp_head' => $wpg_hook('WPGenie_Profiler::wrap_all', PHP_INT_MIN, 0),
	'query' => $wpg_hook('WPGenie_Profiler::count_query', PHP_INT_MIN),
	'wp_redirect' => $wpg_hook('WPGenie_Profiler::redirect', PHP_INT_MAX, 2),
	'shutdown' => $wpg_hook('WPGenie_Profiler::report', PHP_INT_MAX, 0),
];
unset($wpg_hook);

ob_start('WPGenie_Profiler::discard');
require $wpg_root . '/wp-blog-header.php';
