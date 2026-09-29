<?php
/**
 * WPGenie: send wp_mail() through the WPGenie mail server with SMTP AUTH.
 * Loaded by the wpgenie-smtp mu-plugin; the credentials file sits next to
 * wp-config.php (outside the docroot, read-only to PHP).
 */
defined( 'ABSPATH' ) || exit;

$wpgenie_smtp_creds = dirname( ABSPATH ) . '/wpgenie-smtp.php';
if ( is_readable( $wpgenie_smtp_creds ) ) {
	require_once $wpgenie_smtp_creds;
}

if ( defined( 'WPGENIE_SMTP_HOST' ) ) {
	add_action(
		'phpmailer_init',
		static function ( $mail ) {
			$mail->isSMTP();
			$mail->Host       = WPGENIE_SMTP_HOST;
			$mail->Port       = 587;
			$mail->SMTPSecure = 'tls';
			$mail->SMTPAuth   = true;
			$mail->Username   = WPGENIE_SMTP_USER;
			$mail->Password   = WPGENIE_SMTP_PASS;
			// The server only lets this mailbox send as itself (spoof
			// protection). A contact form that sets the visitor as sender
			// keeps working: the visitor becomes Reply-To instead.
			$from = $mail->From;
			if ( $from && 0 !== strcasecmp( $from, WPGENIE_SMTP_USER ) && ! $mail->getReplyToAddresses() ) {
				$mail->addReplyTo( $from, $mail->FromName );
			}
			$mail->setFrom( WPGENIE_SMTP_USER, $mail->FromName, false );
		},
		PHP_INT_MAX
	);
}
