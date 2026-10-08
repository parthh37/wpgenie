<?php
/**
 * WPGenie WordPress hardening. Ships read-only in the PHP image and is
 * loaded by the wpgenie-hardening mu-plugin wrapper WPGenie writes, which
 * defines WPGENIE_HARDEN: the settings the site has on, comma-separated
 * (site.HardeningOptions in the daemon).
 *
 * Everything here is defensive: a hook WordPress doesn't have yet is
 * simply never called, and functions are checked before use, so an old
 * WordPress gets less protection, never a fatal error.
 *
 * WP-CLI is how the panel works on the site (always with plugins skipped):
 * it may do what the locks below refuse everything else. Cron runs through
 * wp-cron.php, never WP-CLI, so a plugin's scheduled job gets no pass.
 */

defined( 'ABSPATH' ) || exit;

if ( ! defined( 'WPGENIE_HARDEN' ) ) {
	return;
}

$wpgenie_harden = array_flip( array_filter( explode( ',', (string) WPGENIE_HARDEN ) ) );

function wpgenie_harden_cli() {
	return defined( 'WP_CLI' ) && WP_CLI;
}

function wpgenie_harden_log( $message ) {
	error_log( 'WPGenie hardening: ' . $message );
}

// ---- Hide usernames ----
if ( isset( $wpgenie_harden['user_enum'] ) ) {
	// ?author=N redirects to the author's archive, whose address is their
	// username: the first thing password-guessing bots ask. REST requests
	// (?author= filters posts there) and wp-admin are left alone.
	add_action(
		'parse_request',
		static function ( $wp ) {
			if ( ! isset( $_GET['author'] ) || is_admin() || ! empty( $wp->query_vars['rest_route'] ) ) {
				return;
			}
			if ( function_exists( 'current_user_can' ) && current_user_can( 'list_users' ) ) {
				return;
			}
			wp_safe_redirect( home_url( '/' ), 301 );
			exit;
		},
		1
	);

	// The users endpoints list usernames (slugs) to anyone. People who
	// write need them (the block editor's author picker); visitors don't.
	add_filter(
		'rest_pre_dispatch',
		static function ( $result, $server = null, $request = null ) {
			if ( null !== $result || ! is_object( $request ) || ! method_exists( $request, 'get_route' ) ) {
				return $result;
			}
			$route = untrailingslashit( (string) $request->get_route() );
			if ( ! preg_match( '#^/wp/v2/users(?:/|$)#', $route ) || '/wp/v2/users/me' === $route ) {
				return $result;
			}
			if ( function_exists( 'current_user_can' ) && current_user_can( 'edit_posts' ) ) {
				return $result;
			}
			$status = function_exists( 'rest_authorization_required_code' ) ? rest_authorization_required_code() : 401;
			return new WP_Error( 'rest_user_cannot_view', 'Sorry, you are not allowed to list users.', array( 'status' => $status ) );
		},
		10,
		3
	);

	// Author sitemaps (WordPress 5.5+) list every author's archive.
	add_filter(
		'wp_sitemaps_add_provider',
		static function ( $provider, $name = '' ) {
			return 'users' === $name ? false : $provider;
		},
		10,
		2
	);

	// Embeds name the author and link their archive.
	add_filter(
		'oembed_response_data',
		static function ( $data ) {
			if ( is_array( $data ) ) {
				unset( $data['author_name'], $data['author_url'] );
			}
			return $data;
		},
		20
	);

	// Comment classes carry the commenting user's nicename.
	add_filter(
		'comment_class',
		static function ( $classes ) {
			if ( ! is_array( $classes ) ) {
				return $classes;
			}
			return array_values(
				array_filter(
					$classes,
					static function ( $c ) {
						return 0 !== strpos( (string) $c, 'comment-author-' );
					}
				)
			);
		}
	);
}

// ---- Vague sign-in errors ----
if ( isset( $wpgenie_harden['login_errors'] ) ) {
	// After every authentication handler (they run at 20, the spam check
	// at 99): which of the two was wrong is never said.
	add_filter(
		'authenticate',
		static function ( $user ) {
			if ( ! is_wp_error( $user ) ) {
				return $user;
			}
			$telling = array( 'invalid_username', 'invalid_email', 'incorrect_password', 'invalidcombo' );
			if ( ! array_intersect( $user->get_error_codes(), $telling ) ) {
				return $user;
			}
			$lost = function_exists( 'wp_lostpassword_url' ) ? wp_lostpassword_url() : '';
			$msg  = '<strong>Error:</strong> The username, e-mail address or password is incorrect.';
			if ( $lost ) {
				$msg .= ' <a href="' . esc_url( $lost ) . '">Lost your password?</a>';
			}
			return new WP_Error( 'authentication_failed', $msg );
		},
		100
	);

	// "Lost your password?" with an unknown name answers like a known one
	// (WordPress 5.4+ says whether a user was found; older ones are left
	// alone rather than guessed at).
	add_action(
		'lostpassword_post',
		static function ( $errors = null, $user_data = 'unknown' ) {
			if ( 'unknown' === $user_data || $user_data || ! is_wp_error( $errors ) ) {
				return;
			}
			if ( empty( $GLOBALS['pagenow'] ) || 'wp-login.php' !== $GLOBALS['pagenow'] ) {
				return;
			}
			$codes = $errors->get_error_codes();
			if ( array_diff( $codes, array( 'invalid_email', 'invalidcombo' ) ) ) {
				return; // nothing typed, or another plugin's error: shown as is
			}
			wp_safe_redirect( add_query_arg( 'checkemail', 'confirm', wp_login_url() ) );
			exit;
		},
		10,
		2
	);
}

// ---- No application passwords (WordPress 5.6+) ----
if ( isset( $wpgenie_harden['app_passwords'] ) ) {
	add_filter( 'wp_is_application_passwords_available', '__return_false' );
}

// ---- Lock administrator accounts ----
if ( isset( $wpgenie_harden['admin_lock'] ) ) {
	$GLOBALS['wpgenie_harden_busy'] = false;

	// Capabilities that make an account an administrator in all but name:
	// whoever has one can make themselves (or anyone) one, or run code.
	function wpgenie_harden_powerful_caps() {
		return array(
			'manage_options', 'promote_users', 'edit_users', 'create_users', 'delete_users',
			'install_plugins', 'activate_plugins', 'edit_plugins', 'delete_plugins',
			'install_themes', 'edit_themes', 'update_core', 'unfiltered_upload',
			'manage_network', 'manage_sites',
		);
	}

	function wpgenie_harden_role_powerful( $caps ) {
		if ( ! is_array( $caps ) ) {
			return false;
		}
		foreach ( wpgenie_harden_powerful_caps() as $cap ) {
			if ( ! empty( $caps[ $cap ] ) ) {
				return true;
			}
		}
		return false;
	}

	// The keys of a user's capabilities (roles, and capabilities granted
	// to them directly) that give administrator powers.
	function wpgenie_harden_powerful_keys( $caps ) {
		global $wp_roles;
		$out = array();
		if ( ! is_array( $caps ) ) {
			return $out;
		}
		$roles = function_exists( 'wp_roles' ) ? wp_roles() : $wp_roles;
		$roles = is_object( $roles ) && isset( $roles->roles ) && is_array( $roles->roles ) ? $roles->roles : array();
		foreach ( $caps as $key => $granted ) {
			$key = (string) $key;
			if ( ! $granted ) {
				continue;
			}
			if ( 'administrator' === $key || in_array( $key, wpgenie_harden_powerful_caps(), true )
				|| ( isset( $roles[ $key ]['capabilities'] ) && wpgenie_harden_role_powerful( $roles[ $key ]['capabilities'] ) ) ) {
				$out[] = $key;
			}
		}
		return $out;
	}

	function wpgenie_harden_meta_key( $key, $suffix ) {
		global $wpdb;
		if ( ! is_string( $key ) || ! isset( $wpdb->base_prefix ) ) {
			return false;
		}
		return 1 === preg_match( '/^' . preg_quote( $wpdb->base_prefix, '/' ) . '(?:\d+_)?' . $suffix . '$/', $key );
	}

	function wpgenie_harden_refused( $what ) {
		wpgenie_harden_log( 'refused ' . $what . ': administrators can only be added from the WPGenie panel while "Lock administrator accounts" is on' );
		if ( function_exists( 'get_current_user_id' ) && function_exists( 'set_transient' ) && get_current_user_id() ) {
			set_transient( 'wpgenie_admin_lock_' . get_current_user_id(), 1, 300 );
		}
	}

	function wpgenie_harden_write_meta( $user_id, $key, $value ) {
		$GLOBALS['wpgenie_harden_busy'] = true;
		$done                           = update_metadata( 'user', $user_id, $key, $value );
		$GLOBALS['wpgenie_harden_busy'] = false;
		return $done;
	}

	// A user's roles and capabilities are one user meta value: whatever
	// writes it (wp-admin, wp_insert_user, a plugin calling
	// update_user_meta directly), administrator powers it adds are taken
	// out before it's stored, and the rest is stored. Powers the account
	// already had stay.
	function wpgenie_harden_guard_meta( $check, $user_id, $meta_key, $meta_value ) {
		if ( null !== $check || ! empty( $GLOBALS['wpgenie_harden_busy'] ) || wpgenie_harden_cli() ) {
			return $check;
		}
		$user_id = (int) $user_id;
		if ( wpgenie_harden_meta_key( $meta_key, 'capabilities' ) ) {
			$new   = maybe_unserialize( $meta_value );
			$added = array_diff( wpgenie_harden_powerful_keys( $new ), wpgenie_harden_powerful_keys( get_user_meta( $user_id, $meta_key, true ) ) );
			if ( ! $added ) {
				return $check;
			}
			foreach ( $added as $key ) {
				unset( $new[ $key ] );
			}
			if ( ! $new ) {
				$new = array( 'subscriber' => true );
			}
			wpgenie_harden_refused( 'giving user ' . $user_id . ' ' . implode( ', ', $added ) );
			return (bool) wpgenie_harden_write_meta( $user_id, $meta_key, $new );
		}
		// The legacy user level WordPress stores next to them (10 for an
		// administrator): only an account with administrator powers in the
		// database keeps one above an editor's.
		if ( wpgenie_harden_meta_key( $meta_key, 'user_level' ) && (int) $meta_value > 7 ) {
			$caps_key = substr( $meta_key, 0, -strlen( 'user_level' ) ) . 'capabilities';
			if ( wpgenie_harden_powerful_keys( get_user_meta( $user_id, $caps_key, true ) ) ) {
				return $check;
			}
			return (bool) wpgenie_harden_write_meta( $user_id, $meta_key, 0 );
		}
		return $check;
	}
	add_filter( 'update_user_metadata', 'wpgenie_harden_guard_meta', 1, 4 );
	add_filter( 'add_user_metadata', 'wpgenie_harden_guard_meta', 1, 4 );

	// Existing roles can't be raised to administrator powers (every
	// subscriber would become one); new roles can be made, and are checked
	// when someone is given them.
	if ( isset( $GLOBALS['wpdb']->prefix ) ) {
		add_filter(
			'pre_update_option_' . $GLOBALS['wpdb']->prefix . 'user_roles',
			static function ( $value, $old = array() ) {
				if ( wpgenie_harden_cli() || ! is_array( $value ) || ! is_array( $old ) ) {
					return $value;
				}
				foreach ( $value as $role => $def ) {
					if ( ! isset( $old[ $role ] ) || 'administrator' === $role ) {
						continue;
					}
					$was = isset( $old[ $role ]['capabilities'] ) ? $old[ $role ]['capabilities'] : array();
					$now = is_array( $def ) && isset( $def['capabilities'] ) ? $def['capabilities'] : array();
					if ( ! wpgenie_harden_role_powerful( $was ) && wpgenie_harden_role_powerful( $now ) ) {
						$value[ $role ] = $old[ $role ];
						wpgenie_harden_refused( 'giving the ' . $role . ' role administrator capabilities' );
					}
				}
				return $value;
			},
			1,
			2
		);
	}

	// New registrations never get administrator powers, whatever the
	// setting says (a favourite of vulnerable plugins: set it, register).
	add_filter(
		'pre_update_option_default_role',
		static function ( $value, $old = 'subscriber' ) {
			if ( wpgenie_harden_cli() || ! wpgenie_harden_powerful_keys( array( (string) $value => true ) ) ) {
				return $value;
			}
			wpgenie_harden_refused( 'making ' . $value . ' the role of new users' );
			return $old;
		},
		1,
		2
	);
	add_filter(
		'option_default_role',
		static function ( $value ) {
			return wpgenie_harden_powerful_keys( array( (string) $value => true ) ) ? 'subscriber' : $value;
		}
	);

	// Multisite: network (super) administrators are a list of usernames.
	// Additions are dropped; removals still happen.
	add_filter(
		'pre_update_site_option_site_admins',
		static function ( $value, $old = array() ) {
			if ( wpgenie_harden_cli() || ! is_array( $value ) ) {
				return $value;
			}
			$old   = is_array( $old ) ? $old : array();
			$added = array_diff( $value, $old );
			if ( ! $added ) {
				return $value;
			}
			wpgenie_harden_refused( 'making ' . implode( ', ', $added ) . ' a network administrator' );
			return array_values( array_intersect( $value, $old ) );
		},
		1,
		2
	);

	// Whoever tried in wp-admin is told why it didn't happen.
	add_action(
		'admin_notices',
		static function () {
			if ( ! function_exists( 'get_current_user_id' ) || ! get_current_user_id() ) {
				return;
			}
			$key = 'wpgenie_admin_lock_' . get_current_user_id();
			if ( ! get_transient( $key ) ) {
				return;
			}
			delete_transient( $key );
			echo '<div class="notice notice-error"><p>' . esc_html( 'Administrators can only be added from your hosting panel: the administrator lock is on (Protection, WordPress hardening). Nobody was given administrator rights.' ) . '</p></div>';
		}
	);
}

// ---- No plugin or theme installs from wp-admin ----
if ( isset( $wpgenie_harden['file_mods'] ) ) {
	// DISALLOW_FILE_MODS, except for WP-CLI (the panel's updates) and
	// WordPress's own background security updates.
	add_filter(
		'file_mod_allowed',
		static function ( $allowed, $context = '' ) {
			return wpgenie_harden_cli() || 'automatic_updater' === $context ? $allowed : false;
		},
		10,
		2
	);
	// WordPress before 4.8 has no file_mod_allowed: the capabilities.
	add_filter(
		'map_meta_cap',
		static function ( $caps, $cap = '' ) {
			$mods = array(
				'install_plugins', 'upload_plugins', 'update_plugins', 'delete_plugins',
				'install_themes', 'upload_themes', 'update_themes', 'delete_themes',
				'install_languages', 'update_languages', 'update_core', 'edit_plugins', 'edit_themes',
			);
			if ( ! wpgenie_harden_cli() && is_array( $caps ) && in_array( $cap, $mods, true ) ) {
				$caps[] = 'do_not_allow';
			}
			return $caps;
		},
		10,
		2
	);
}

// ---- Shorter administrator sign-ins ----
if ( isset( $wpgenie_harden['short_sessions'] ) ) {
	add_filter(
		'auth_cookie_expiration',
		static function ( $length, $user_id = 0, $remember = false ) {
			$user_id = (int) $user_id;
			if ( $user_id <= 0 || ! function_exists( 'user_can' ) ) {
				return $length;
			}
			$admin = user_can( $user_id, 'manage_options' ) || ( function_exists( 'is_super_admin' ) && is_super_admin( $user_id ) );
			if ( ! $admin ) {
				return $length;
			}
			return min( (int) $length, $remember ? 3 * DAY_IN_SECONDS : 12 * HOUR_IN_SECONDS );
		},
		99,
		3
	);
}

// ---- Strong passwords ----
if ( isset( $wpgenie_harden['strong_passwords'] ) ) {
	// wpgenie_harden_weak says what's wrong with a password, or ''.
	function wpgenie_harden_weak( $pass, $user ) {
		$pass = (string) $pass;
		if ( strlen( $pass ) < 12 ) {
			return 'Choose a password of at least 12 characters.';
		}
		$classes = 0;
		foreach ( array( '/[a-z]/', '/[A-Z]/', '/[0-9]/', '/[^a-zA-Z0-9]/' ) as $re ) {
			$classes += preg_match( $re, $pass );
		}
		if ( $classes < 2 ) {
			return 'Mix letters with numbers or symbols in the password.';
		}
		if ( count( array_unique( str_split( $pass ) ) ) < 5 ) {
			return 'The password repeats the same few characters: choose another.';
		}

		$flat     = strtolower( preg_replace( '/[^a-zA-Z0-9]+/', '', $pass ) );
		$personal = array();
		if ( is_object( $user ) ) {
			foreach ( array( 'user_login', 'user_nicename', 'display_name' ) as $f ) {
				if ( ! empty( $user->$f ) ) {
					$personal[] = (string) $user->$f;
				}
			}
			if ( ! empty( $user->user_email ) ) {
				$personal[] = strstr( (string) $user->user_email, '@', true );
			}
		}
		if ( function_exists( 'get_bloginfo' ) ) {
			$personal[] = (string) get_bloginfo( 'name' );
		}
		if ( function_exists( 'home_url' ) ) {
			$host = (string) ( function_exists( 'wp_parse_url' ) ? wp_parse_url( home_url(), PHP_URL_HOST ) : parse_url( home_url(), PHP_URL_HOST ) );
			$host = preg_replace( '/^www\./', '', $host );
			$personal[] = (string) strtok( $host, '.' );
		}
		foreach ( $personal as $p ) {
			$p = strtolower( preg_replace( '/[^a-zA-Z0-9]+/', '', (string) $p ) );
			if ( strlen( $p ) >= 4 && false !== strpos( $flat, $p ) ) {
				return "The password can't contain the username, e-mail address or the site's name.";
			}
		}

		// A common word with digits or symbols around it ("Password123!"),
		// letters swapped for look-alikes inside it included ("P@ssw0rd").
		$core = preg_replace( '/^[^a-zA-Z]+|[^a-zA-Z]+$/', '', $pass );
		$core = strtolower( strtr( (string) $core, array( '0' => 'o', '1' => 'i', '3' => 'e', '4' => 'a', '5' => 's', '7' => 't', '@' => 'a', '$' => 's', '!' => 'i' ) ) );
		$common = array(
			'password', 'passwort', 'motdepasse', 'contrasena', 'qwerty', 'qwertyuiop', 'qwertz', 'azerty', 'asdfgh', 'asdfghjkl',
			'zxcvbnm', 'letmein', 'welcome', 'admin', 'administrator', 'root', 'login', 'user', 'iloveyou', 'monkey', 'dragon',
			'sunshine', 'princess', 'football', 'baseball', 'soccer', 'superman', 'batman', 'starwars', 'pokemon', 'master',
			'shadow', 'whatever', 'trustno', 'secret', 'changeme', 'default', 'hello', 'helloworld', 'freedom', 'computer',
			'internet', 'summer', 'winter', 'spring', 'autumn', 'wordpress', 'website', 'mywebsite', 'abc', 'abcdef', 'abcdefgh',
			'michael', 'jennifer', 'charlie', 'jordan', 'liverpool', 'chelsea', 'arsenal', 'access', 'mustang', 'killer',
			'qazwsx', 'qazwsxedc', 'passpass', 'test', 'testing', 'demo', 'guest',
		);
		$keyboard = array( '1q2w3e4r5t6y', '1qaz2wsx3edc', 'q1w2e3r4t5y6', '123qweasdzxc', 'qazwsxedcrfv', 'zaq12wsxcde3', '1234qwerasdf' );
		$lower    = strtolower( $pass );
		if ( '' === $core || in_array( $core, $common, true ) || in_array( $lower, $keyboard, true ) ) {
			return 'That password is one of the most common ones: choose another.';
		}
		return '';
	}

	function wpgenie_harden_writes( $user_id, $role = '' ) {
		if ( $role && function_exists( 'get_role' ) ) {
			$r = get_role( $role );
			if ( $r && $r->has_cap( 'edit_posts' ) ) {
				return true;
			}
		}
		return $user_id > 0 && function_exists( 'user_can' ) && user_can( $user_id, 'edit_posts' );
	}

	// Profile and new-user screens in wp-admin: the new password is still
	// in plain text here (edit_user() hashes it afterwards).
	add_action(
		'user_profile_update_errors',
		static function ( $errors, $update = false, $user = null ) {
			if ( wpgenie_harden_cli() || ! is_wp_error( $errors ) || ! is_object( $user ) || empty( $user->user_pass ) ) {
				return;
			}
			$id = isset( $user->ID ) ? (int) $user->ID : 0;
			if ( ! wpgenie_harden_writes( $id, isset( $user->role ) ? (string) $user->role : '' ) ) {
				return;
			}
			$who = $user;
			if ( empty( $who->user_login ) && $id > 0 && function_exists( 'get_userdata' ) ) {
				$stored = get_userdata( $id );
				if ( $stored ) {
					$who = (object) array(
						'user_login'    => $stored->user_login,
						'user_nicename' => $stored->user_nicename,
						'display_name'  => $stored->display_name,
						'user_email'    => ! empty( $user->user_email ) ? $user->user_email : $stored->user_email,
					);
				}
			}
			$problem = wpgenie_harden_weak( (string) $user->user_pass, $who );
			if ( '' !== $problem ) {
				$errors->add( 'pass', '<strong>Error:</strong> ' . esc_html( $problem ), array( 'form-field' => 'pass1' ) );
			}
		},
		10,
		3
	);

	// "Lost your password?" → the new password form.
	add_action(
		'validate_password_reset',
		static function ( $errors, $user = null ) {
			if ( ! is_wp_error( $errors ) || ! is_object( $user ) || is_wp_error( $user ) || empty( $_POST['pass1'] ) || ! is_string( $_POST['pass1'] ) ) {
				return;
			}
			if ( ! wpgenie_harden_writes( (int) $user->ID ) ) {
				return;
			}
			$problem = wpgenie_harden_weak( wp_unslash( $_POST['pass1'] ), $user );
			if ( '' !== $problem ) {
				$errors->add( 'password_reset_weak', '<strong>Error:</strong> ' . esc_html( $problem ) );
			}
		},
		10,
		2
	);
}

// ---- No pingbacks or trackbacks ----
if ( isset( $wpgenie_harden['pingbacks'] ) ) {
	// Receiving: closed everywhere, and gone from XML-RPC (which the
	// shield may allow for Jetpack and the apps).
	add_filter( 'pings_open', '__return_false', 99 );
	add_filter(
		'xmlrpc_methods',
		static function ( $methods ) {
			if ( is_array( $methods ) ) {
				unset( $methods['pingback.ping'], $methods['pingback.extensions.getPingbacks'] );
			}
			return $methods;
		}
	);
	add_filter(
		'wp_headers',
		static function ( $headers ) {
			if ( is_array( $headers ) ) {
				unset( $headers['X-Pingback'] );
			}
			return $headers;
		}
	);
	add_filter(
		'bloginfo_url',
		static function ( $output, $show = '' ) {
			return 'pingback_url' === $show ? '' : $output;
		},
		10,
		2
	);
	add_filter(
		'pre_option_default_ping_status',
		static function () {
			return 'closed';
		}
	);
	// Sending: no pingbacks, no trackbacks.
	add_filter(
		'pre_option_default_pingback_flag',
		static function () {
			return '0';
		}
	);
	add_action(
		'pre_ping',
		static function ( &$links ) {
			$links = array();
		}
	);
	add_filter( 'get_to_ping', '__return_empty_array' );
}
