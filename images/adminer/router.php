<?php
// Only WPGenie's front (which checks the daemon's secret) is served: not
// the image's own index.php or adminer.php, reachable from the sites'
// Docker network otherwise.
$path = parse_url( $_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH );
if ( ! is_string( $path ) || strpos( $path, '/_wpgenie/adminer/' ) !== 0 ) {
	http_response_code( 404 );
	exit;
}
require '/var/www/html/_wpgenie/adminer/index.php';
