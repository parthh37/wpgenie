<?php
/**
 * WPGenie maintenance mode. Ships read-only in the PHP image and is loaded
 * by the wpgenie-maintenance mu-plugin wrapper WPGenie writes while a site
 * is in maintenance, which defines WPGENIE_MAINTENANCE: the message for
 * visitors, base64-encoded ("" for the default one).
 *
 * Visitors get a plain page saying the site is down for maintenance: HTTP
 * 503 with Retry-After, so search engines come back later instead of
 * indexing it. People signed in who can edit posts see the site as usual,
 * with a reminder in the admin bar. wp-admin, wp-login.php, WP-Cron, WP-CLI
 * and the REST API for signed-in users keep working; WPGenie's own
 * /_wpgenie/ addresses never reach WordPress.
 *
 * Nothing is cached meanwhile: WPGenie empties the page cache when
 * maintenance starts and ends, and DONOTCACHEPAGE stops the page cache
 * storing what editors see.
 */

defined( 'ABSPATH' ) || exit;

if ( ! defined( 'WPGENIE_MAINTENANCE' ) ) {
	return;
}

defined( 'DONOTCACHEPAGE' ) || define( 'DONOTCACHEPAGE', true );

// Seconds visitors (and crawlers) are asked to wait before trying again.
const WPGENIE_MAINTENANCE_RETRY = 600;

function wpgenie_maintenance_message(): string {
	$msg = base64_decode( (string) WPGENIE_MAINTENANCE, true );
	$msg = is_string( $msg ) ? trim( $msg ) : '';
	return $msg !== '' ? $msg : "We're doing some maintenance on this site. Please check back soon.";
}

/** Whether this request sees the site as usual. */
function wpgenie_maintenance_passes(): bool {
	if ( ( defined( 'WP_CLI' ) && WP_CLI ) || wp_doing_cron() || is_admin() ) {
		return true;
	}
	$path = parse_url( (string) ( $_SERVER['REQUEST_URI'] ?? '/' ), PHP_URL_PATH );
	if ( is_string( $path ) && str_starts_with( $path, '/_wpgenie/' ) ) {
		return true;
	}
	// WPGenie's health checks (the shield checked their token) see the real
	// site, so an update made during maintenance is still judged, and
	// rolled back if it broke the site. Anyone could copy the marks and see
	// the site too: maintenance hides it, the password lock protects it.
	if ( isset( $_GET['wpgenie-health'] ) && ! empty( $_SERVER['HTTP_X_WPGENIE_HEALTH'] ) ) {
		return true;
	}
	return is_user_logged_in() && current_user_can( 'edit_posts' );
}

/** Visitors' page: before anything else on template_redirect (the page cache starts at 0). */
function wpgenie_maintenance_page(): void {
	if ( wpgenie_maintenance_passes() ) {
		return;
	}
	status_header( 503 );
	nocache_headers();
	header( 'Retry-After: ' . WPGENIE_MAINTENANCE_RETRY );
	header( 'Content-Type: text/html; charset=utf-8' );
	header( 'X-Robots-Tag: noindex' );
	if ( ( $_SERVER['REQUEST_METHOD'] ?? 'GET' ) === 'HEAD' ) {
		exit;
	}
	$name = trim( wp_strip_all_tags( (string) get_bloginfo( 'name' ) ) );
	$lang = (string) get_bloginfo( 'language' );
	$msg  = nl2br( esc_html( wpgenie_maintenance_message() ), false );
	?>
<!DOCTYPE html>
<html lang="<?php echo esc_attr( $lang !== '' ? $lang : 'en' ); ?>">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title><?php echo esc_html( $name !== '' ? $name . ' – Maintenance' : 'Maintenance' ); ?></title>
<style>
html{color-scheme:light dark}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px;box-sizing:border-box;
font:17px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:#f5f5f7;color:#1d1d1f}
main{max-width:34rem;text-align:center}
h1{font-size:1.75rem;line-height:1.2;margin:0 0 .75rem;font-weight:650;letter-spacing:-.01em}
p{margin:0;color:#515154}
@media (prefers-color-scheme:dark){body{background:#1c1c1e;color:#f5f5f7}p{color:#a1a1a6}}
</style>
</head>
<body>
<main>
<h1><?php echo esc_html( $name !== '' ? $name : 'Down for maintenance' ); ?></h1>
<p><?php echo $msg; // phpcs:ignore WordPress.Security.EscapeOutput -- escaped above ?></p>
</main>
</body>
</html>
	<?php
	exit;
}
add_action( 'template_redirect', 'wpgenie_maintenance_page', -1000 );

// The REST API answers signed-in users only (the block editor, the
// mobile apps with application passwords). Runs after WordPress's own
// cookie and application password checks.
add_filter(
	'rest_authentication_errors',
	static function ( $result ) {
		if ( is_wp_error( $result ) || is_user_logged_in() ) {
			return $result;
		}
		return new WP_Error( 'wpgenie_maintenance', wpgenie_maintenance_message(), array( 'status' => 503 ) );
	},
	1000
);

// A reminder for those who still see the site.
add_action(
	'admin_bar_menu',
	static function ( $bar ): void {
		if ( ! current_user_can( 'edit_posts' ) ) {
			return;
		}
		$bar->add_node(
			array(
				'id'    => 'wpgenie-maintenance',
				'title' => 'Maintenance mode is on',
				'meta'  => array( 'title' => 'Visitors see a maintenance page. Turn it off in the hosting panel (Tools).' ),
			)
		);
	},
	100
);
add_action(
	'admin_notices',
	static function (): void {
		if ( current_user_can( 'edit_posts' ) ) {
			echo '<div class="notice notice-warning"><p><strong>Maintenance mode is on:</strong> visitors see a maintenance page. Turn it off in the hosting panel, under Tools.</p></div>';
		}
	}
);
