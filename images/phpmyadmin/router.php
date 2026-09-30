<?php
// Every request passes here first. Only the WPGenie daemon may use this
// server: it proves itself with the secret it wrote into the container's
// tmpfs (the sites share its Docker network). Then only phpMyAdmin's
// entry points and static files are served: not its libraries,
// templates or vendor code.
$wpg_secret = @file_get_contents( '/run/wpg/secret' );
$wpg_given  = $_SERVER['HTTP_X_WPGENIE_SECRET'] ?? '';
if ( ! is_string( $wpg_secret ) || strlen( $wpg_secret ) < 32 || ! is_string( $wpg_given ) || ! hash_equals( $wpg_secret, $wpg_given ) ) {
	http_response_code( 403 );
	exit( 'Forbidden' );
}
unset( $_SERVER['HTTP_X_WPGENIE_SECRET'] );

$wpg_prefix = '/_wpgenie/phpmyadmin/';
$wpg_path   = parse_url( $_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH );
if ( ! is_string( $wpg_path ) || strpos( $wpg_path, $wpg_prefix ) !== 0 ) {
	http_response_code( 404 );
	exit;
}
$wpg_rel = substr( $wpg_path, strlen( $wpg_prefix ) );
if ( in_array( $wpg_rel, array( '', 'index.php', 'url.php', 'js/messages.php', 'favicon.ico' ), true ) ) {
	return false; // the built-in server runs or sends it
}
if ( preg_match( '#^(?:js|themes|doc/html)/[A-Za-z0-9._/-]+$#', $wpg_rel ) && strpos( $wpg_rel, '..' ) === false
	&& ! preg_match( '#\.(?:php\d?|phtml|phar|inc|twig)$#i', $wpg_rel ) ) {
	return false;
}
http_response_code( 404 );
exit;
