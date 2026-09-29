<?php
/**
 * WPGenie pull-zone CDN (Bunny, or any CDN that fetches from the site on its
 * own hostname). Ships read-only in the PHP image and is loaded by the
 * mu-plugin wrapper WPGenie writes, which defines WPGENIE_CDN_URL
 * ("https://cdn.example.com").
 *
 * Links to the site's static files (under wp-content and wp-includes) in
 * front-end HTML are rewritten to the CDN's hostname; the CDN fetches them
 * from here once and serves them from near each visitor. Runs inside the
 * page cache's buffer, so cached pages carry the CDN links too (WPGenie
 * purges them when the CDN hostname changes).
 */

defined( 'ABSPATH' ) || exit;

const WPGENIE_CDN_EXT = 'css|js|mjs|png|jpe?g|gif|webp|avif|svg|ico|woff2?|ttf|otf|eot|mp4|webm|mp3|pdf';

/**
 * Rewrites links to static files on $hosts (and root-relative ones) to
 * $cdn. A link is only rewritten inside an attribute value, a CSS url() or
 * a srcset list: it must start right after a quote, "(", "," or white space.
 * JSON-escaped links (\/) are left alone.
 */
function wpgenie_cdn_rewrite( string $html, array $hosts, string $cdn ): string {
	if ( $hosts === [] || ! preg_match( '#^https://[a-z0-9.-]+$#i', $cdn ) ) {
		return $html;
	}
	$h  = implode( '|', array_map( static fn( $x ) => preg_quote( $x, '#' ), $hosts ) );
	$re = '#(?<=["\'(\s,])(?:(?:https?:)?//(?:' . $h . '))?(/(?:wp-content|wp-includes)/[^"\'\s()<>?\#]+\.(?:' . WPGENIE_CDN_EXT . '))(?=[?"\'\s),\#])#i';
	return (string) preg_replace_callback(
		$re,
		static function ( array $m ) use ( $cdn ): string {
			// Never a PHP script, whatever its URL ends in.
			return stripos( $m[1], '.php' ) !== false ? $m[0] : $cdn . $m[1];
		},
		$html
	);
}

function wpgenie_cdn_start(): void {
	if ( ! defined( 'WPGENIE_CDN_URL' ) || is_admin() || is_customize_preview() || is_feed()
		|| ( defined( 'REST_REQUEST' ) && REST_REQUEST ) ) {
		return;
	}
	$hosts = [];
	foreach ( [ home_url(), site_url() ] as $url ) {
		$host = wp_parse_url( $url, PHP_URL_HOST );
		if ( is_string( $host ) && $host !== '' ) {
			$hosts[] = strtolower( $host );
		}
	}
	$hosts = array_values( array_unique( $hosts ) );
	ob_start(
		static function ( string $html ) use ( $hosts ): string {
			foreach ( headers_list() as $header ) {
				$h = strtolower( $header );
				if ( str_starts_with( $h, 'content-type:' ) && ! str_contains( $h, 'text/html' ) ) {
					return $html;
				}
			}
			return wpgenie_cdn_rewrite( $html, $hosts, (string) WPGENIE_CDN_URL );
		}
	);
}
// After the page cache's buffer (priority 0): this one is inside it, so
// the stored page is the rewritten one.
add_action( 'template_redirect', 'wpgenie_cdn_start', 1 );
