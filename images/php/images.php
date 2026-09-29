<?php
/**
 * WPGenie image optimisation. Ships read-only in the PHP image and is loaded
 * by the mu-plugin wrapper WPGenie writes when the site converts images; the
 * wrapper defines WPGENIE_IMAGE_FORMATS ("avif,webp", "webp", …).
 *
 * New uploads get AVIF/WebP copies next to each size, made by cron (the
 * system cron runs every minute) rather than during the upload: AVIF
 * encoding takes a second or more per size. Deleting an upload deletes its
 * copies. The daemon converts what is already there, and nightly catches
 * files that arrived another way (SFTP, restores).
 */

defined( 'ABSPATH' ) || exit;

require_once __DIR__ . '/image-convert.php';

function wpgenie_image_formats(): array {
	$want = defined( 'WPGENIE_IMAGE_FORMATS' ) ? explode( ',', (string) WPGENIE_IMAGE_FORMATS ) : [];
	return wpgenie_image_formats_supported( array_intersect( WPGENIE_IMG_FORMATS, $want ) );
}

/** The files of an attachment: the original, the scaled copy and every size. */
function wpgenie_image_attachment_files( int $id ): array {
	$file = get_attached_file( $id );
	$meta = wp_get_attachment_metadata( $id );
	if ( ! is_string( $file ) || $file === '' ) {
		return [];
	}
	$dir   = dirname( $file );
	$files = [ $file ];
	if ( is_array( $meta ) ) {
		if ( ! empty( $meta['original_image'] ) ) {
			$files[] = $dir . '/' . basename( (string) $meta['original_image'] );
		}
		foreach ( (array) ( $meta['sizes'] ?? [] ) as $size ) {
			if ( ! empty( $size['file'] ) ) {
				$files[] = $dir . '/' . basename( (string) $size['file'] );
			}
		}
	}
	return array_values( array_unique( $files ) );
}

add_filter(
	'wp_generate_attachment_metadata',
	static function ( $meta, $id ) {
		if ( wpgenie_image_formats() && wp_attachment_is_image( $id ) ) {
			wp_schedule_single_event( time(), 'wpgenie_convert_images', [ (int) $id ] );
		}
		return $meta;
	},
	99,
	2
);

add_action(
	'wpgenie_convert_images',
	static function ( $id ): void {
		$formats = wpgenie_image_formats();
		foreach ( wpgenie_image_attachment_files( (int) $id ) as $file ) {
			wpgenie_convert_image( $file, $formats );
		}
	}
);

// WordPress deletes every size of a deleted (or re-cropped) image with
// wp_delete_file(): take the copies along.
add_filter(
	'wp_delete_file',
	static function ( $file ) {
		if ( is_string( $file ) && preg_match( WPGENIE_IMG_SOURCE, $file ) ) {
			wpgenie_image_forget( $file );
		}
		return $file;
	}
);
