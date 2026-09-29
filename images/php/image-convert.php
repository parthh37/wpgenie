<?php
/**
 * WPGenie image conversion: writes <file>.avif and <file>.webp next to a
 * JPEG or PNG upload. Caddy serves them in place of the original to browsers
 * that accept them (the URL, and so the HTML and the page cache, never
 * change). Plain PHP with GD, no WordPress: used by images.php (new uploads,
 * from cron) and convert-images.php (the whole uploads directory).
 */

const WPGENIE_IMG_FORMATS = [ 'avif', 'webp' ];
const WPGENIE_IMG_SOURCE  = '/\.(?:jpe?g|png)$/i';
// Decoded, a picture takes 4 bytes a pixel: stay well inside PHP's memory.
const WPGENIE_IMG_MAX_PIXELS = 16000000;
const WPGENIE_IMG_MAX_BYTES  = 32 << 20;
// Visually close to the originals WordPress itself makes (JPEG 82).
const WPGENIE_IMG_QUALITY = [ 'webp' => 80, 'avif' => 55 ];

/** The formats this PHP can write, of those asked for. */
function wpgenie_image_formats_supported( array $formats ): array {
	$gd = function_exists( 'gd_info' ) ? gd_info() : [];
	return array_values(
		array_filter(
			$formats,
			static fn( $f ) => ( $f === 'webp' && ! empty( $gd['WebP Support'] ) && function_exists( 'imagewebp' ) )
				|| ( $f === 'avif' && ! empty( $gd['AVIF Support'] ) && function_exists( 'imageavif' ) )
		)
	);
}

/**
 * Converts one upload into each format (skipping copies newer than the
 * original). Returns what happened per format: converted, current, larger
 * (the copy wasn't smaller, so none is kept), or skipped: <why>.
 */
function wpgenie_convert_image( string $src, array $formats ): array {
	$out = [];
	if ( ! preg_match( WPGENIE_IMG_SOURCE, $src ) || is_link( $src ) || ! is_file( $src ) ) {
		return [ 'all' => 'skipped: not a JPEG/PNG file' ];
	}
	$size  = filesize( $src );
	$mtime = filemtime( $src );
	$todo  = [];
	foreach ( $formats as $f ) {
		$dst = $src . '.' . $f;
		// A copy carries its original's timestamp: any other means the
		// original changed since (replaced over SFTP, restored, edited).
		if ( is_file( $dst ) && ! is_link( $dst ) && filemtime( $dst ) === $mtime ) {
			$out[ $f ] = 'current';
		} else {
			$todo[] = $f;
		}
	}
	if ( ! $todo ) {
		return $out;
	}
	$info = @getimagesize( $src );
	if ( ! $info || $size > WPGENIE_IMG_MAX_BYTES || $info[0] * $info[1] > WPGENIE_IMG_MAX_PIXELS ) {
		return $out + [ 'all' => 'skipped: too large or unreadable' ];
	}
	// CMYK JPEGs decode with wrong colours; animated PNGs would lose all but
	// their first frame.
	if ( ( $info[2] === IMAGETYPE_JPEG && ( $info['channels'] ?? 3 ) === 4 )
		|| ( $info[2] === IMAGETYPE_PNG && wpgenie_png_is_animated( $src ) ) ) {
		return $out + [ 'all' => 'skipped: CMYK or animated' ];
	}
	$im = $info[2] === IMAGETYPE_JPEG ? @imagecreatefromjpeg( $src ) : ( $info[2] === IMAGETYPE_PNG ? @imagecreatefrompng( $src ) : false );
	if ( ! $im ) {
		return $out + [ 'all' => 'skipped: GD could not read it' ];
	}
	if ( $info[2] === IMAGETYPE_JPEG ) {
		$im = wpgenie_image_orient( $im, $src );
	} else {
		imagepalettetotruecolor( $im );
		imagealphablending( $im, false );
		imagesavealpha( $im, true );
	}
	foreach ( $todo as $f ) {
		$dst = $src . '.' . $f;
		$tmp = dirname( $src ) . '/.' . basename( $dst ) . '.' . bin2hex( random_bytes( 4 ) ) . '.tmp';
		$ok  = $f === 'webp' ? @imagewebp( $im, $tmp, WPGENIE_IMG_QUALITY['webp'] ) : @imageavif( $im, $tmp, WPGENIE_IMG_QUALITY['avif'], 6 );
		clearstatcache( true, $tmp );
		if ( ! $ok || ! is_file( $tmp ) || filesize( $tmp ) === 0 ) {
			$out[ $f ] = 'skipped: encoding failed';
		} elseif ( filesize( $tmp ) >= $size ) {
			// No gain: serve the original. A stale copy must not stay either.
			@unlink( $dst );
			$out[ $f ] = 'larger';
		} elseif ( @rename( $tmp, $dst ) ) {
			@touch( $dst, $mtime );
			$out[ $f ] = 'converted';
		} else {
			$out[ $f ] = 'skipped: could not write';
		}
		@unlink( $tmp );
	}
	imagedestroy( $im );
	return $out;
}

/**
 * Applies a JPEG's EXIF orientation. Browsers rotate the original by it,
 * and GD drops it, so an unrotated copy would show sideways.
 */
function wpgenie_image_orient( GdImage $im, string $src ): GdImage {
	$o = function_exists( 'exif_read_data' ) ? (int) ( @exif_read_data( $src )['Orientation'] ?? 1 ) : 1;
	// WordPress's own sequence (WP_Image_Editor::maybe_exif_rotate), so a
	// copy matches the sizes WordPress generated: turn (degrees
	// counter-clockwise, as imagerotate), then flip.
	[ $angle, $flip ] = [
		2 => [ 0, IMG_FLIP_HORIZONTAL ],
		3 => [ 180, 0 ],
		4 => [ 0, IMG_FLIP_VERTICAL ],
		5 => [ 90, IMG_FLIP_VERTICAL ],
		6 => [ 270, 0 ],
		7 => [ 90, IMG_FLIP_HORIZONTAL ],
		8 => [ 90, 0 ],
	][ $o ] ?? [ 0, 0 ];
	if ( $angle !== 0 ) {
		$rotated = imagerotate( $im, $angle, 0 );
		if ( $rotated ) {
			imagedestroy( $im );
			$im = $rotated;
		}
	}
	if ( $flip !== 0 ) {
		imageflip( $im, $flip );
	}
	return $im;
}

/** APNGs carry an acTL chunk before their image data. */
function wpgenie_png_is_animated( string $src ): bool {
	$head = (string) @file_get_contents( $src, false, null, 0, 4096 );
	$idat = strpos( $head, 'IDAT' );
	$actl = strpos( $head, 'acTL' );
	return $actl !== false && ( $idat === false || $actl < $idat );
}

/** Removes the converted copies of an upload (it was deleted or replaced). */
function wpgenie_image_forget( string $src ): void {
	foreach ( WPGENIE_IMG_FORMATS as $f ) {
		if ( is_file( $src . '.' . $f ) || is_link( $src . '.' . $f ) ) {
			@unlink( $src . '.' . $f );
		}
	}
}
