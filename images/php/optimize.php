<?php
/**
 * WPGenie performance tweaks. Ships read-only in the PHP image and is
 * loaded by the wpgenie-optimize mu-plugin wrapper WPGenie writes, which
 * defines WPGENIE_OPTIMIZE: the tweaks the site has on, comma-separated
 * (site.Optimizations in the daemon). Each one only removes work: nothing
 * here stores anything or changes content.
 */

defined( 'ABSPATH' ) || exit;

if ( ! defined( 'WPGENIE_OPTIMIZE' ) ) {
	return;
}

$wpgenie_optimize = array_flip( array_filter( explode( ',', (string) WPGENIE_OPTIMIZE ) ) );

// Emoji: browsers draw emoji themselves.
if ( isset( $wpgenie_optimize['emoji'] ) ) {
	remove_action( 'wp_head', 'print_emoji_detection_script', 7 );
	remove_action( 'admin_print_scripts', 'print_emoji_detection_script' );
	remove_action( 'wp_print_styles', 'print_emoji_styles' );
	remove_action( 'admin_print_styles', 'print_emoji_styles' );
	remove_action( 'wp_enqueue_scripts', 'wp_enqueue_emoji_styles' );
	remove_action( 'admin_enqueue_scripts', 'wp_enqueue_emoji_styles' );
	remove_filter( 'the_content_feed', 'wp_staticize_emoji' );
	remove_filter( 'comment_text_rss', 'wp_staticize_emoji' );
	remove_filter( 'wp_mail', 'wp_staticize_emoji_for_email' );
	add_filter( 'emoji_svg_url', '__return_false' );
	add_filter(
		'tiny_mce_plugins',
		static fn( $plugins ) => is_array( $plugins ) ? array_values( array_diff( $plugins, array( 'wpemoji' ) ) ) : $plugins
	);
}

// oEmbed discovery: other sites embedding this one. Embedding others'
// content in posts keeps working.
if ( isset( $wpgenie_optimize['embeds'] ) ) {
	remove_action( 'wp_head', 'wp_oembed_add_discovery_links' );
	remove_action( 'wp_head', 'wp_oembed_add_host_js' );
	add_action( 'wp_footer', static fn() => wp_dequeue_script( 'wp-embed' ) );
}

// <head> clutter; the generator tag also advertises the WordPress version.
if ( isset( $wpgenie_optimize['head'] ) ) {
	remove_action( 'wp_head', 'rsd_link' );
	remove_action( 'wp_head', 'wlwmanifest_link' );
	remove_action( 'wp_head', 'wp_generator' );
	remove_action( 'wp_head', 'wp_shortlink_wp_head' );
	remove_action( 'template_redirect', 'wp_shortlink_header', 11 );
	add_filter( 'the_generator', '__return_empty_string' );
}

// Heartbeat: every 60 s instead of 15 in wp-admin; not on visitors' pages
// (dequeued only: a script that needs it still gets it as a dependency).
if ( isset( $wpgenie_optimize['heartbeat'] ) ) {
	add_filter(
		'heartbeat_settings',
		static function ( $settings ) {
			$settings['interval'] = 60;
			return $settings;
		}
	);
	add_action(
		'wp_enqueue_scripts',
		static function (): void {
			if ( ! is_user_logged_in() ) {
				wp_dequeue_script( 'heartbeat' );
			}
		},
		100
	);
}

// Pingbacks to this site's own posts.
if ( isset( $wpgenie_optimize['self_pings'] ) ) {
	add_action(
		'pre_ping',
		static function ( &$links ): void {
			$home = home_url();
			foreach ( $links as $i => $link ) {
				if ( str_starts_with( (string) $link, $home ) ) {
					unset( $links[ $i ] );
				}
			}
		}
	);
}

// Dashicons: the admin icon font, for signed-in users (the admin bar) only.
if ( isset( $wpgenie_optimize['dashicons'] ) ) {
	add_action(
		'wp_enqueue_scripts',
		static function (): void {
			if ( ! is_user_logged_in() ) {
				wp_dequeue_style( 'dashicons' );
			}
		},
		100
	);
}

// jQuery Migrate on visitors' pages (wp-admin keeps it).
if ( isset( $wpgenie_optimize['jquery_migrate'] ) ) {
	add_action(
		'wp_default_scripts',
		static function ( $scripts ): void {
			if ( is_admin() || empty( $scripts->registered['jquery'] ) ) {
				return;
			}
			$scripts->registered['jquery']->deps = array_values( array_diff( $scripts->registered['jquery']->deps, array( 'jquery-migrate' ) ) );
		}
	);
}
