<?php
/**
 * WPGenie full-page cache. Ships read-only in the PHP image and is loaded by
 * the mu-plugin wrapper WPGenie writes when the page cache is enabled.
 *
 * WordPress renders a page once; this stores the HTML at
 * wp-content/cache/wpgenie/<path>/index.html and Caddy serves that file to
 * later anonymous visitors without starting PHP. Caddy repeats the request
 * checks below (GET/HEAD, no query string, no session cookies, path ending
 * in "/") with a cookie rule that is a superset of ours, so a cached page is
 * only ever served to a request that could have produced it.
 *
 * Any content change purges the whole cache: always correct, and warming it
 * again costs one PHP render per page.
 *
 * Pages whose HTML depends on the device (the theme or a plugin asked
 * wp_is_mobile() while rendering) are stored twice, as index-mobile.html and
 * index-desktop.html, and Caddy picks one with wp_is_mobile()'s own rule
 * (WPGENIE_CACHE_MOBILE_UA). Sites whose theme sniffs the User-Agent itself
 * set WPGENIE_CACHE_MOBILE_ALWAYS (panel: "separate mobile cache") to store
 * every page that way.
 */

defined( 'ABSPATH' ) || exit;

const WPGENIE_CACHE_DIR    = WP_CONTENT_DIR . '/cache/wpgenie';
const WPGENIE_CACHE_MARKER = WP_CONTENT_DIR . '/cache/wpgenie.purged';
// Below WordPress's 12h nonce tick, so a cached form never carries an
// expired nonce. Pruned hourly: worst-case age is ~11h.
const WPGENIE_CACHE_TTL = 10 * HOUR_IN_SECONDS;
// Never *serve* a cached page to a request with these cookies. Mirrored (as a
// substring match, i.e. stricter) in the Caddy config.
const WPGENIE_CACHE_BYPASS_COOKIES = '/^(wordpress_logged_in_|wordpress_sec_|wp-postpass_|comment_author_|woocommerce_items_in_cart|woocommerce_cart_hash|wp_woocommerce_session_|edd_items_in_cart|PHPSESSID)/';
// Only *store* pages rendered for requests whose cookies are all known not to
// change the HTML (analytics: Google, Meta, Hotjar, Matomo, Clarity, Bing;
// WordPress's test cookie). Any
// other cookie might personalise the page (recently viewed products, a cart,
// a session), and a stored page is served to everyone.
const WPGENIE_CACHE_STORE_COOKIES = '/^(_ga|_gid|_gat|_gcl_|_fbp|_fbc|_hj|_pk_|_clck|_clsk|_uet|__utm|wordpress_test_cookie$|wp_lang$)/';
// wp_is_mobile()'s User-Agent rule. Mirrored exactly in the Caddy config
// (proxy.MobileUA), which picks the copy to serve with it.
const WPGENIE_CACHE_MOBILE_UA = '#Mobile|Android|Silk/|Kindle|BlackBerry|Opera Mini|Opera Mobi#';

/**
 * Whether Caddy treats this request as mobile: the Sec-CH-UA-Mobile client
 * hint when it is sent, else the User-Agent. Deliberately not
 * wp_is_mobile(), which plugins can filter: storing must follow the rule
 * Caddy serves by.
 */
function wpgenie_cache_is_mobile(): bool {
	$hint = (string) ( $_SERVER['HTTP_SEC_CH_UA_MOBILE'] ?? '' );
	if ( $hint !== '' ) {
		return $hint === '?1';
	}
	return (bool) preg_match( WPGENIE_CACHE_MOBILE_UA, (string) ( $_SERVER['HTTP_USER_AGENT'] ?? '' ) );
}

// Set when anything asks wp_is_mobile() during the render: the page may
// differ between phones and computers.
$GLOBALS['wpgenie_cache_device'] = false;
add_filter(
	'wp_is_mobile',
	static function ( $is_mobile ) {
		$GLOBALS['wpgenie_cache_device'] = true;
		return $is_mobile;
	},
	PHP_INT_MAX
);

/**
 * The URL path this request would be cached under, or null if the request
 * must not be cached. Caddy looks files up by the decoded path, so decode
 * the same way and refuse anything that could leave the cache directory.
 */
function wpgenie_cache_path(): ?string {
	if ( ( $_SERVER['REQUEST_METHOD'] ?? '' ) !== 'GET' || ( $_SERVER['QUERY_STRING'] ?? '' ) !== '' ) {
		return null;
	}
	if ( isset( $_SERVER['HTTP_AUTHORIZATION'] ) || isset( $_SERVER['PHP_AUTH_USER'] ) ) {
		return null;
	}
	foreach ( array_keys( $_COOKIE ) as $name ) {
		if ( ! preg_match( WPGENIE_CACHE_STORE_COOKIES, (string) $name ) ) {
			return null; // covers every bypass cookie too
		}
	}
	$path = parse_url( $_SERVER['REQUEST_URI'] ?? '', PHP_URL_PATH );
	if ( ! is_string( $path ) ) {
		return null;
	}
	$path = rawurldecode( $path );
	if ( $path === '' || $path[0] !== '/' || substr( $path, -1 ) !== '/' || strlen( $path ) > 512
		|| str_contains( $path, '//' ) || str_contains( $path, '/.' ) || preg_match( '/[\x00-\x1f\x7f\\\\]/', $path ) ) {
		return null;
	}
	return $path;
}

function wpgenie_cache_start(): void {
	$path = wpgenie_cache_path();
	if ( $path === null || is_user_logged_in() || is_admin() || is_feed() || is_preview()
		|| ( defined( 'DONOTCACHEPAGE' ) && DONOTCACHEPAGE ) ) {
		header( 'X-WPGenie-Cache: BYPASS' );
		return;
	}
	header( 'X-WPGenie-Cache: MISS' );
	ob_start(
		static function ( string $html, int $phase ) use ( $path ): string {
			// Only when the whole page arrives in one call: if something
			// flushed early, $html is just the tail of the page.
			$whole = PHP_OUTPUT_HANDLER_START | PHP_OUTPUT_HANDLER_FINAL;
			if ( ( $phase & $whole ) === $whole ) {
				wpgenie_cache_store( $path, $html );
			}
			return $html;
		}
	);
}
add_action( 'template_redirect', 'wpgenie_cache_start', 0 );

function wpgenie_cache_store( string $path, string $html ): void {
	// Themes and plugins (WooCommerce cart/checkout, …) can opt out mid-render.
	if ( ( defined( 'DONOTCACHEPAGE' ) && DONOTCACHEPAGE ) || http_response_code() !== 200
		|| session_status() === PHP_SESSION_ACTIVE ) {
		return;
	}
	foreach ( headers_list() as $header ) {
		$h = strtolower( $header );
		if ( str_starts_with( $h, 'set-cookie:' )
			|| ( str_starts_with( $h, 'content-type:' ) && ! str_contains( $h, 'text/html' ) )
			|| ( str_starts_with( $h, 'cache-control:' ) && preg_match( '/no-cache|no-store|private/', $h ) ) ) {
			return;
		}
	}
	if ( stripos( $html, '</html>' ) === false ) {
		return; // fatal error or truncated output
	}
	$name = 'index.html';
	if ( $GLOBALS['wpgenie_cache_device'] || ( defined( 'WPGENIE_CACHE_MOBILE_ALWAYS' ) && WPGENIE_CACHE_MOBILE_ALWAYS ) ) {
		$mobile = wpgenie_cache_is_mobile();
		// A filtered wp_is_mobile() that disagrees with the rule Caddy
		// serves by would put this page in front of the other device type.
		if ( wp_is_mobile() !== $mobile ) {
			return;
		}
		$name = $mobile ? 'index-mobile.html' : 'index-desktop.html';
	}
	// A purge that happened while this page was rendering means the HTML may
	// already be stale: don't resurrect it.
	clearstatcache( true, WPGENIE_CACHE_MARKER );
	$purged = @filemtime( WPGENIE_CACHE_MARKER );
	if ( $purged !== false && $purged >= (int) ( $_SERVER['REQUEST_TIME'] ?? 0 ) ) {
		return;
	}
	$dir = WPGENIE_CACHE_DIR . $path;
	if ( ! wp_mkdir_p( $dir ) ) {
		return;
	}
	$out = $html . "\n<!-- WPGenie page cache: " . gmdate( 'c' ) . " -->\n";
	// Compressed copies first: Caddy only looks for them once the page
	// itself exists, and a copy that can't be made must not be left stale.
	$copies = [ '.gz' => gzencode( $out, 9 ) ];
	$copies['.br'] = function_exists( 'brotli_compress' ) ? brotli_compress( $out, 11, BROTLI_TEXT ) : false;
	foreach ( $copies as $ext => $data ) {
		if ( ! is_string( $data ) || ! wpgenie_cache_write( $dir . $name . $ext, $data ) ) {
			@unlink( $dir . $name . $ext );
		}
	}
	if ( wpgenie_cache_write( $dir . $name, $out ) && $name !== 'index.html' ) {
		// Now device-specific: an older copy for every device must not win.
		@unlink( $dir . 'index.html' );
	}
}

/** Write-then-rename: Caddy never serves a half-written file. */
function wpgenie_cache_write( string $file, string $data ): bool {
	$tmp = dirname( $file ) . '/.' . basename( $file ) . '.' . bin2hex( random_bytes( 6 ) ) . '.tmp';
	$ok  = @file_put_contents( $tmp, $data ) === strlen( $data ) && @rename( $tmp, $file );
	@unlink( $tmp );
	return $ok;
}

function wpgenie_cache_purge(): void {
	if ( ! wp_mkdir_p( dirname( WPGENIE_CACHE_MARKER ) ) ) {
		return;
	}
	@touch( WPGENIE_CACHE_MARKER );
	if ( ! is_dir( WPGENIE_CACHE_DIR ) ) {
		return;
	}
	// Renaming first makes the purge atomic for Caddy; deleting can then take
	// its time.
	$trash = WPGENIE_CACHE_DIR . '.trash-' . bin2hex( random_bytes( 6 ) );
	wpgenie_cache_rmtree( @rename( WPGENIE_CACHE_DIR, $trash ) ? $trash : WPGENIE_CACHE_DIR );
}

function wpgenie_cache_rmtree( string $dir, ?int $older_than = null ): void {
	if ( ! is_dir( $dir ) ) {
		return;
	}
	$items = new RecursiveIteratorIterator(
		new RecursiveDirectoryIterator( $dir, FilesystemIterator::SKIP_DOTS ),
		RecursiveIteratorIterator::CHILD_FIRST
	);
	foreach ( $items as $item ) {
		if ( $item->isDir() && ! $item->isLink() ) {
			if ( $older_than === null ) {
				@rmdir( $item->getPathname() );
			}
		} elseif ( $older_than === null || $item->getMTime() < $older_than ) {
			@unlink( $item->getPathname() );
		}
	}
	if ( $older_than === null ) {
		@rmdir( $dir );
	}
}

/** Hourly (via WPGenie's system cron): expire old pages and leftover trash. */
function wpgenie_cache_prune(): void {
	wpgenie_cache_rmtree( WPGENIE_CACHE_DIR, time() - WPGENIE_CACHE_TTL );
	foreach ( glob( WPGENIE_CACHE_DIR . '.trash-*', GLOB_ONLYDIR ) ?: [] as $trash ) {
		wpgenie_cache_rmtree( $trash );
	}
}
add_action( 'wpgenie_cache_prune', 'wpgenie_cache_prune' );
if ( wp_doing_cron() && ! wp_next_scheduled( 'wpgenie_cache_prune' ) ) {
	wp_schedule_event( time() + 60, 'hourly', 'wpgenie_cache_prune' );
}

// --- Purge triggers -------------------------------------------------------
// Developers can call do_action( 'wpgenie_purge_cache' ) from their own code.
// These hooks also fire for WP-CLI: --skip-plugins still loads mu-plugins.
add_action( 'wpgenie_purge_cache', 'wpgenie_cache_purge' );

// "Purge cache" in the admin bar, for anyone who may edit others' content:
// for changes no hook sees (a page builder's global block, a plugin's own
// settings). Purging touches the marker, so the CDN follows (the daemon
// watches it).
const WPGENIE_PURGE_CAP = 'edit_others_posts';
add_action(
	'admin_bar_menu',
	static function ( $bar ): void {
		if ( ! current_user_can( WPGENIE_PURGE_CAP ) ) {
			return;
		}
		$back = ( is_admin() ? '' : home_url( wp_unslash( $_SERVER['REQUEST_URI'] ?? '/' ) ) );
		$bar->add_node(
			[
				'id'    => 'wpgenie-purge',
				'title' => 'Purge cache',
				'href'  => wp_nonce_url( add_query_arg( [ 'action' => 'wpgenie_purge', 'back' => rawurlencode( $back ) ], admin_url( 'admin-post.php' ) ), 'wpgenie_purge' ),
				'meta'  => [ 'title' => 'Empty the page cache, object cache and CDN (WPGenie)' ],
			]
		);
	},
	100
);
add_action(
	'admin_post_wpgenie_purge',
	static function (): void {
		check_admin_referer( 'wpgenie_purge' );
		if ( ! current_user_can( WPGENIE_PURGE_CAP ) ) {
			wp_die( 'You are not allowed to purge the cache.', 403 );
		}
		wpgenie_cache_purge();
		if ( wp_using_ext_object_cache() ) {
			wp_cache_flush(); // this site's keys only (WP_REDIS_SELECTIVE_FLUSH)
		}
		// wp_safe_redirect() only goes to this site's own hosts.
		$back = isset( $_GET['back'] ) ? wp_unslash( (string) $_GET['back'] ) : '';
		wp_safe_redirect( $back !== '' ? $back : add_query_arg( 'wpgenie-purged', '1', wp_get_referer() ?: admin_url() ) );
		exit;
	}
);
add_action(
	'admin_notices',
	static function (): void {
		if ( isset( $_GET['wpgenie-purged'] ) && current_user_can( WPGENIE_PURGE_CAP ) ) {
			echo '<div class="notice notice-success is-dismissible"><p>Cache purged: pages are rendered afresh on their next visit.</p></div>';
		}
	}
);

add_action(
	'transition_post_status',
	static function ( $new, $old, $post ): void {
		// Published, unpublished or edited while live; drafts never reach the cache.
		if ( ( $new === 'publish' || $old === 'publish' ) && is_post_type_viewable( $post->post_type ) ) {
			wpgenie_cache_purge();
		}
	},
	10,
	3
);
add_action(
	'transition_comment_status',
	static function ( $new, $old ): void {
		if ( $new === 'approved' || $old === 'approved' ) {
			wpgenie_cache_purge();
		}
	},
	10,
	2
);
add_action(
	'comment_post',
	static function ( $id, $approved ): void {
		if ( $approved === 1 ) {
			wpgenie_cache_purge();
		}
	},
	10,
	2
);
foreach ( [
	'edit_comment', 'switch_theme', 'customize_save_after', 'wp_update_nav_menu', 'wp_delete_nav_menu',
	'update_option_sidebars_widgets', 'update_option_blogname', 'update_option_blogdescription',
	'update_option_permalink_structure', 'update_option_page_on_front', 'update_option_show_on_front',
	'created_term', 'edited_term', 'delete_term', 'activated_plugin', 'deactivated_plugin',
	'upgrader_process_complete', 'woocommerce_product_set_stock', 'woocommerce_variation_set_stock',
] as $wpgenie_hook ) {
	add_action( $wpgenie_hook, 'wpgenie_cache_purge', 99, 0 );
}
unset( $wpgenie_hook );
