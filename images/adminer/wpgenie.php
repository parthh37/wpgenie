<?php
/**
 * WPGenie's front for Adminer. Only the WPGenie daemon may use it: it
 * proves itself with the secret it wrote into this container's tmpfs and
 * passes the temporary database account of the session in headers. The
 * login form can't be used to reach any other server: every login is
 * replaced by the session's account.
 */
$wpg_secret = @file_get_contents( '/run/wpg/secret' );
$wpg_given  = $_SERVER['HTTP_X_WPGENIE_ADMINER'] ?? '';
if ( ! is_string( $wpg_secret ) || strlen( $wpg_secret ) < 32 || ! hash_equals( $wpg_secret, $wpg_given ) ) {
	http_response_code( 403 );
	exit( 'Forbidden' );
}
$wpg_db = array(
	'driver'   => 'server',
	'server'   => (string) ( $_SERVER['HTTP_X_WPGENIE_DB_HOST'] ?? '' ),
	'username' => (string) ( $_SERVER['HTTP_X_WPGENIE_DB_USER'] ?? '' ),
	'password' => (string) ( $_SERVER['HTTP_X_WPGENIE_DB_PASS'] ?? '' ),
	'db'       => (string) ( $_SERVER['HTTP_X_WPGENIE_DB_NAME'] ?? '' ),
);
unset( $_SERVER['HTTP_X_WPGENIE_ADMINER'], $_SERVER['HTTP_X_WPGENIE_DB_PASS'] );
// Not logged in to the session's account yet (first request, after
// Adminer's logout, or a URL naming another server/user): log in to it.
if ( isset( $_POST['auth'] ) || ( $_GET['username'] ?? null ) !== $wpg_db['username'] || ( $_GET['server'] ?? '' ) !== $wpg_db['server'] ) {
	$_POST = array( 'auth' => $wpg_db );
	$_GET  = array();
	$_SERVER['REQUEST_METHOD'] = 'POST';
}
chdir( '/var/www/html' );
require '/var/www/html/adminer.php';
