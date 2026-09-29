<?php
/**
 * WPGenie: convert every JPEG/PNG under a site's uploads to the given
 * formats, and remove copies that no longer belong (original gone, format
 * turned off). Run by the daemon inside the site's container, as the site
 * user, under the same PHP jail as cron:
 *
 *   php convert-images.php <uploads dir> <formats, e.g. avif,webp or "">
 *
 * Prints one JSON line per 50 files ({"done":n,"total":m}) and a summary
 * line at the end ({"summary":{...}}). Never loads WordPress: no site code
 * runs.
 */

if ( PHP_SAPI !== 'cli' ) {
	exit( 1 );
}
require __DIR__ . '/image-convert.php';

[ , $dir, $want ] = $argv + [ '', '', '' ];
if ( $dir === '' || ! is_dir( $dir ) || is_link( $dir ) ) {
	fwrite( STDERR, "no uploads directory\n" );
	exit( 0 ); // nothing uploaded yet: nothing to do
}
$formats = wpgenie_image_formats_supported( array_intersect( WPGENIE_IMG_FORMATS, explode( ',', $want ) ) );
@ini_set( 'memory_limit', '256M' );

// Symlinked directories are not followed: they may lead out of the site.
$walk = static function () use ( $dir ): Generator {
	$it = new RecursiveIteratorIterator(
		new RecursiveDirectoryIterator( $dir, FilesystemIterator::SKIP_DOTS | FilesystemIterator::CURRENT_AS_PATHNAME ),
		RecursiveIteratorIterator::LEAVES_ONLY,
		RecursiveIteratorIterator::CATCH_GET_CHILD
	);
	foreach ( $it as $path ) {
		yield (string) $path;
	}
};

$sources = [];
$copies  = [];
foreach ( $walk() as $path ) {
	if ( preg_match( '/\.(?:jpe?g|png)\.(avif|webp)$/i', $path ) ) {
		$copies[] = $path;
	} elseif ( preg_match( WPGENIE_IMG_SOURCE, $path ) && ! is_link( $path ) ) {
		$sources[] = $path;
	}
}

$sum = [ 'images' => count( $sources ), 'converted' => 0, 'current' => 0, 'larger' => 0, 'skipped' => 0,
	'removed' => 0, 'bytes_before' => 0, 'bytes_after' => 0, 'formats' => $formats ];

// Copies whose original is gone, or of a format no longer wanted.
foreach ( $copies as $copy ) {
	$ext  = strtolower( pathinfo( $copy, PATHINFO_EXTENSION ) );
	$orig = substr( $copy, 0, -strlen( $ext ) - 1 );
	if ( ! in_array( $ext, $formats, true ) || ! is_file( $orig ) || is_link( $orig ) ) {
		if ( @unlink( $copy ) ) {
			$sum['removed']++;
		}
	}
}

$total = count( $sources );
foreach ( $sources as $i => $src ) {
	if ( $i % 50 === 0 ) {
		echo json_encode( [ 'done' => $i, 'total' => $total ] ), "\n";
	}
	if ( ! $formats ) {
		break;
	}
	$res = wpgenie_convert_image( $src, $formats );
	foreach ( $res as $f => $r ) {
		$key = str_starts_with( $r, 'skipped' ) ? 'skipped' : $r;
		$sum[ $key ] = ( $sum[ $key ] ?? 0 ) + 1;
	}
	clearstatcache();
	$best = $size = (int) @filesize( $src );
	foreach ( $formats as $f ) {
		if ( is_file( $src . '.' . $f ) ) {
			$best = min( $best, (int) filesize( $src . '.' . $f ) );
		}
	}
	$sum['bytes_before'] += $size;
	$sum['bytes_after']  += $best;
}
echo json_encode( [ 'done' => $total, 'total' => $total ] ), "\n";
echo json_encode( [ 'summary' => $sum ] ), "\n";
