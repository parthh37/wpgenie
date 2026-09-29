<?php
/**
 * WPGenie uploads offload. Ships read-only in the PHP image and is loaded by
 * the mu-plugin wrapper WPGenie writes for sites whose uploads are copied to
 * object storage. The wrapper defines:
 *
 *   WPGENIE_OFFLOAD_UPLOADS  the uploads directory (absolute)
 *   WPGENIE_OFFLOAD_QUEUE    the delete queue, in the site's logs/ directory
 *   WPGENIE_OFFLOAD_POKE     touched after uploads and deletes
 *
 * PHP never holds the storage's keys: it only notes, one path per line
 * relative to the uploads directory, which uploads WordPress deleted. The
 * daemon reads the queue (as untrusted input), deletes those objects from
 * the bucket and empties it. Appends take an exclusive lock, which the
 * daemon also holds while it reads and truncates, so no line is lost. The
 * poke file lets new uploads reach the bucket within seconds.
 */

defined( 'ABSPATH' ) || exit;

/** Queues files (absolute paths) for deletion from the bucket. */
function wpgenie_offload_queue( array $files ): void {
	if ( ! defined( 'WPGENIE_OFFLOAD_UPLOADS' ) || ! defined( 'WPGENIE_OFFLOAD_QUEUE' ) ) {
		return;
	}
	$base  = rtrim( wp_normalize_path( (string) WPGENIE_OFFLOAD_UPLOADS ), '/' ) . '/';
	$lines = '';
	foreach ( array_unique( $files ) as $file ) {
		if ( ! is_string( $file ) || $file === '' ) {
			continue;
		}
		$file = wp_normalize_path( $file );
		if ( ! str_starts_with( $file, $base ) ) {
			continue; // not an upload (or a custom UPLOADS directory WPGenie doesn't copy)
		}
		$rel = substr( $file, strlen( $base ) );
		if ( $rel === '' || preg_match( '/[\x00-\x1f\x7f]/', $rel ) ) {
			continue; // a line break would split the entry
		}
		$lines .= $rel . "\n";
	}
	if ( $lines === '' ) {
		return;
	}
	// file_put_contents with LOCK_EX appends under flock(LOCK_EX).
	@file_put_contents( (string) WPGENIE_OFFLOAD_QUEUE, $lines, FILE_APPEND | LOCK_EX );
	wpgenie_offload_poke();
}

function wpgenie_offload_poke(): void {
	if ( defined( 'WPGENIE_OFFLOAD_POKE' ) ) {
		@touch( (string) WPGENIE_OFFLOAD_POKE );
	}
}

/**
 * Every file of an attachment: the attached file, the original of a scaled
 * image, every size, and the sizes kept from earlier edits.
 */
function wpgenie_offload_attachment_files( int $id ): array {
	$file = get_attached_file( $id, true );
	if ( ! is_string( $file ) || $file === '' ) {
		return [];
	}
	$dir   = dirname( $file );
	$files = [ $file ];
	$meta  = wp_get_attachment_metadata( $id, true );
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
	$backups = get_post_meta( $id, '_wp_attachment_backup_sizes', true );
	if ( is_array( $backups ) ) {
		foreach ( $backups as $size ) {
			if ( ! empty( $size['file'] ) ) {
				$files[] = $dir . '/' . basename( (string) $size['file'] );
			}
		}
	}
	return $files;
}

// Before WordPress deletes the files: once a local copy has been removed
// (local_days), WordPress skips files it can't find and wp_delete_file never
// runs for them, so the attachment's own list is what counts.
add_action(
	'delete_attachment',
	static function ( $id ): void {
		wpgenie_offload_queue( wpgenie_offload_attachment_files( (int) $id ) );
	}
);

// Every other deletion of an upload (re-cropped sizes, plugins).
add_filter(
	'wp_delete_file',
	static function ( $file ) {
		if ( is_string( $file ) ) {
			wpgenie_offload_queue( [ $file ] );
		}
		return $file;
	}
);

// A new upload (and its sizes) is complete: copy it soon.
add_filter(
	'wp_generate_attachment_metadata',
	static function ( $meta ) {
		wpgenie_offload_poke();
		return $meta;
	},
	100
);
add_filter(
	'wp_handle_upload',
	static function ( $upload ) {
		wpgenie_offload_poke();
		return $upload;
	}
);
