<?php
/**
 * WPGenie's phpMyAdmin configuration. The router has already checked the
 * daemon's secret; the daemon passes the session's temporary database
 * account in headers, and phpMyAdmin connects with it (auth_type config).
 * There is no login form: no other server or account can be reached.
 */
$wpg_header = static function ( string $name ): string {
	$v = $_SERVER[ 'HTTP_X_WPGENIE_' . $name ] ?? '';
	return is_string( $v ) ? $v : '';
};
$wpg_db = $wpg_header( 'DB_NAME' );

$cfg['Servers'][1] = array(
	'auth_type'       => 'config',
	'host'            => $wpg_header( 'DB_HOST' ),
	'user'            => $wpg_header( 'DB_USER' ),
	'password'        => $wpg_header( 'DB_PASS' ),
	'only_db'         => $wpg_db,
	'compress'        => false,
	'AllowNoPassword' => false,
	'AllowRoot'       => false,
);
unset( $_SERVER['HTTP_X_WPGENIE_DB_PASS'] );
$cfg['ServerDefault']        = 1;
$cfg['AllowArbitraryServer'] = false;

// Cookie encryption key, per container start (derived from its secret).
$cfg['blowfish_secret'] = hash( 'sha256', 'phpmyadmin cookies ' . (string) @file_get_contents( '/run/wpg/secret' ), true );

// Behind the daemon, on the site's own HTTPS origin.
$cfg['PmaAbsoluteUri'] = 'https://' . preg_replace( '/[^A-Za-z0-9.:\[\]-]/', '', (string) ( $_SERVER['HTTP_HOST'] ?? '' ) ) . '/_wpgenie/phpmyadmin/';
$cfg['TempDir']        = '/tmp';
$cfg['ExecTimeLimit']  = 300; // the daemon's write timeout is 5 minutes
$cfg['MemoryLimit']    = '256M';

// Nothing that calls out, writes to the WordPress database on its own, or
// can't work with a temporary account on one database.
$cfg['VersionCheck']                      = false;
$cfg['SendErrorReports']                  = 'never';
$cfg['ZeroConf']                          = false; // would create pma__ tables in the site's database
$cfg['PmaNoRelation_DisableWarning']      = true;
$cfg['ShowCreateDb']                      = false;
$cfg['ShowChgPassword']                   = false;
$cfg['ShowServerInfo']                    = false;
$cfg['ShowPhpInfo']                       = false;
$cfg['ShowGitRevision']                   = false;
$cfg['LoginCookieValidityDisableWarning'] = true;
$cfg['UploadDir']                         = '';
$cfg['SaveDir']                           = '';
