<?php
/**
 * WPGenie branding: WordPress's admin shows the host's brand instead of
 * WordPress's. Ships read-only in the PHP image and is loaded by the
 * wpgenie-brand mu-plugin wrapper WPGenie writes, which defines
 * WPGENIE_BRAND_NAME, WPGENIE_BRAND_URL and WPGENIE_BRAND_LOGO (a
 * root-relative URL the WPGenie daemon serves on the site's own domain, or
 * "" for a name without a logo).
 *
 * - Login page: the brand's logo (or name) instead of WordPress's, linking
 *   to the brand, and the brand in the page title.
 * - Admin bar: the WordPress logo menu (links to wordpress.org, WordPress
 *   news) becomes the brand's logo linking to the brand.
 * - Admin: "Thank you for creating with WordPress" and "WordPress" in page
 *   titles become the brand; no WordPress Events and News widget, no
 *   welcome panel.
 */

defined( 'ABSPATH' ) || exit;

if ( ! defined( 'WPGENIE_BRAND_NAME' ) ) {
	return;
}

function wpgenie_brand_name(): string {
	return trim( (string) WPGENIE_BRAND_NAME );
}

function wpgenie_brand_url(): string {
	$url = (string) WPGENIE_BRAND_URL;
	return '' !== $url ? $url : home_url( '/' );
}

function wpgenie_brand_logo(): string {
	$logo = (string) WPGENIE_BRAND_LOGO;
	return str_starts_with( $logo, '/_wpgenie/' ) ? $logo : '';
}

// --- Login page.

add_action(
	'login_enqueue_scripts',
	static function (): void {
		$logo = wpgenie_brand_logo();
		if ( '' !== $logo ) {
			$css = sprintf(
				'.login h1 a{background-image:url("%s")!important;background-size:contain!important;background-position:center!important;width:100%%!important;max-width:320px;height:84px!important}',
				esc_url( $logo )
			);
		} else {
			// No logo: the name, as text, where the logo was.
			$css = '.login h1 a{background-image:none!important;text-indent:0!important;width:auto!important;height:auto!important;'
				. 'font-size:24px;line-height:1.3;font-weight:600;color:#1d2327;text-decoration:none}';
		}
		wp_add_inline_style( 'login', $css );
	}
);

add_filter( 'login_headerurl', static fn() => wpgenie_brand_url() );

add_filter(
	'login_headertext',
	static function ( $text ) {
		$name = wpgenie_brand_name();
		return '' !== $name ? $name : $text;
	}
);

// --- Admin bar: the brand in place of WordPress's logo menu.

add_action(
	'admin_bar_menu',
	static function ( $bar ): void {
		$bar->remove_node( 'wp-logo' );
		$name  = wpgenie_brand_name();
		$logo  = wpgenie_brand_logo();
		$title = '' !== $logo
			? sprintf( '<img src="%s" alt="%s" class="wpgenie-brand-logo">', esc_url( $logo ), esc_attr( $name ) )
			: esc_html( $name );
		if ( '' === $title ) {
			return;
		}
		$bar->add_node(
			array(
				'id'    => 'wpgenie-brand',
				'title' => $title,
				'href'  => wpgenie_brand_url(),
				'meta'  => array( 'target' => '_blank', 'rel' => 'noopener', 'title' => $name ),
			)
		);
	},
	11 // After WordPress adds its own (priority 10).
);

$wpgenie_brand_bar_css = static function (): void {
	if ( ! is_admin_bar_showing() ) {
		return;
	}
	wp_add_inline_style(
		'admin-bar',
		'#wpadminbar #wp-admin-bar-wpgenie-brand>.ab-item{display:flex;align-items:center;padding:0 8px}'
		. '#wpadminbar #wp-admin-bar-wpgenie-brand img.wpgenie-brand-logo{max-height:20px;max-width:120px;width:auto;height:auto;vertical-align:middle}'
	);
};
add_action( 'wp_enqueue_scripts', $wpgenie_brand_bar_css );
add_action( 'admin_enqueue_scripts', $wpgenie_brand_bar_css );

// --- Admin screens.

add_filter(
	'admin_footer_text',
	static function ( $text ) {
		$name = wpgenie_brand_name();
		if ( '' === $name ) {
			return '';
		}
		return sprintf( '%s <a href="%s" target="_blank" rel="noopener">%s</a>', esc_html__( 'Hosted by', 'wpgenie' ), esc_url( wpgenie_brand_url() ), esc_html( $name ) );
	}
);

$wpgenie_brand_title = static function ( $title ) {
	$name = wpgenie_brand_name();
	return '' !== $name ? str_replace( 'WordPress', $name, (string) $title ) : $title;
};
add_filter( 'admin_title', $wpgenie_brand_title );
add_filter( 'login_title', $wpgenie_brand_title );

add_action(
	'wp_dashboard_setup',
	static function (): void {
		remove_meta_box( 'dashboard_primary', 'dashboard', 'side' ); // WordPress Events and News
		// Added by wp-admin's own filters, which load after mu-plugins.
		remove_action( 'welcome_panel', 'wp_welcome_panel' );
	}
);
