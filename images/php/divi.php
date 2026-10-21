<?php
/**
 * WPGenie Divi license: the host's Elegant Themes account, applied to Divi
 * without being stored in the site's database or shown in Divi's settings.
 * Ships read-only in the PHP image and is loaded by the wpgenie-divi
 * mu-plugin wrapper WPGenie writes into sites that have Divi. The
 * credentials file (WPGENIE_ET_USER, WPGENIE_ET_KEY) sits next to
 * wp-config.php: outside the docroot, read-only to PHP.
 *
 * - Divi reads its account from the et_automatic_updates_options option:
 *   the pre_option filters answer with the host's, so theme updates and
 *   premade layouts work.
 * - Nothing is ever written to that option (a save keeps the old value; an
 *   add is undone at once), and a copy stored before WPGenie managed the
 *   license is deleted on the next admin page.
 * - On Divi's Theme Options screen the key is replaced with asterisks
 *   before the page is drawn, so it is never printed into the form, and a
 *   notice says the license is managed by the host.
 */

defined( 'ABSPATH' ) || exit;

$wpgenie_divi_creds = dirname( ABSPATH ) . '/wpgenie-divi.php';
if ( is_readable( $wpgenie_divi_creds ) ) {
	require_once $wpgenie_divi_creds;
}
unset( $wpgenie_divi_creds );

if ( ! defined( 'WPGENIE_ET_USER' ) || ! defined( 'WPGENIE_ET_KEY' ) ) {
	return;
}

const WPGENIE_ET_OPTION = 'et_automatic_updates_options';

/**
 * Whether the key is masked: from the moment Divi's Theme Options screen
 * starts drawing (Divi's updater has read the real one by then).
 */
function wpgenie_divi_masked( ?bool $set = null ): bool {
	static $masked = false;
	if ( null !== $set ) {
		$masked = $set;
	}
	return $masked;
}

/** The account Divi gets. */
function wpgenie_divi_account(): array {
	return array(
		'username' => (string) WPGENIE_ET_USER,
		'api_key'  => wpgenie_divi_masked() ? str_repeat( '*', 24 ) : (string) WPGENIE_ET_KEY,
	);
}

/** Divi's Theme Options screen (wp-admin/admin.php?page=et_divi_options). */
function wpgenie_divi_options_screen(): bool {
	// phpcs:ignore WordPress.Security.NonceVerification.Recommended -- only compared.
	return is_admin() && isset( $_GET['page'] ) && 'et_divi_options' === $_GET['page'];
}

// Reading: the host's account (get_option and, for Divi's multisite-aware
// code, get_site_option).
add_filter( 'pre_option_' . WPGENIE_ET_OPTION, 'wpgenie_divi_account', PHP_INT_MAX );
add_filter( 'pre_site_option_' . WPGENIE_ET_OPTION, 'wpgenie_divi_account', PHP_INT_MAX );

// Writing: never. update_option() sees the value unchanged and stores
// nothing (the old value is the filtered account, never a stored row).
add_filter(
	'pre_update_option_' . WPGENIE_ET_OPTION,
	static function ( $value, $old_value ) {
		return $old_value;
	},
	PHP_INT_MAX,
	2
);
add_filter(
	'pre_update_site_option_' . WPGENIE_ET_OPTION,
	static function ( $value, $old_value ) {
		return $old_value;
	},
	PHP_INT_MAX,
	2
);

// add_option() has no filter to refuse with: undo it right after. Deleting
// doesn't add or update anything, so this can't loop.
add_action(
	'add_option_' . WPGENIE_ET_OPTION,
	static function () {
		delete_option( WPGENIE_ET_OPTION );
	}
);
add_action(
	'add_site_option_' . WPGENIE_ET_OPTION,
	static function () {
		if ( is_multisite() ) {
			delete_site_option( WPGENIE_ET_OPTION );
		}
	}
);

// A key stored before WPGenie managed the license (typed into Divi, or
// in an imported database) mustn't linger: delete_option() is one indexed
// lookup when there is nothing to delete, and never calls the filters
// above.
add_action(
	'admin_init',
	static function () {
		delete_option( WPGENIE_ET_OPTION );
		if ( is_multisite() ) {
			delete_site_option( WPGENIE_ET_OPTION );
		}
	}
);

// Divi's Theme Options: the form shows the username and asterisks.
add_action(
	'in_admin_header',
	static function () {
		if ( wpgenie_divi_options_screen() ) {
			wpgenie_divi_masked( true );
		}
	}
);

add_action(
	'admin_notices',
	static function () {
		if ( ! wpgenie_divi_options_screen() || ! current_user_can( 'edit_theme_options' ) ) {
			return;
		}
		printf(
			'<div class="notice notice-info"><p>%s</p></div>',
			esc_html__( 'Your Divi license is managed by your host: updates and premade layouts work without entering a username or API key here.', 'wpgenie' )
		);
	}
);
