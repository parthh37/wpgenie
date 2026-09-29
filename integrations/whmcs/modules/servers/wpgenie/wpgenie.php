<?php
/**
 * WPGenie provisioning module for WHMCS.
 *
 * Each WHMCS service is one WPGenie account (a customer, or a reseller)
 * on a WPGenie plan. The module talks to WPGenie's provisioning API over
 * HTTPS with a WPGenie API token (the server's "Password" or "Access
 * Hash" field): create a token for an administrator (or a reseller, to
 * provision only their own customers) with `wpgenie token create`.
 *
 * Accounts are found by their WHMCS service ID, which WPGenie stores; the
 * creation carries an idempotency key, so a retried or repeated
 * CreateAccount never makes a second account.
 *
 * @see https://developers.whmcs.com/provisioning-modules/
 */

use WHMCS\Database\Capsule;

if (!defined('WHMCS')) {
    die('This file cannot be accessed directly');
}

const WPGENIE_MODULE = 'wpgenie';
const WPGENIE_MIN_PASSWORD = 12;

function wpgenie_MetaData()
{
    return [
        'DisplayName' => 'WPGenie',
        'APIVersion' => '1.1',
        'RequiresServer' => true,
        'DefaultSSLPort' => '443',
        'ServiceSingleSignOnLabel' => 'Log in to WPGenie',
        'AdminSingleSignOnLabel' => 'Log in to WPGenie as this client',
    ];
}

function wpgenie_ConfigOptions()
{
    return [
        'Plan' => [
            'Type' => 'text',
            'Size' => '32',
            'Description' => 'WPGenie plan ID (wpgenie plan ls)',
        ],
        'Account type' => [
            'Type' => 'dropdown',
            'Options' => 'customer,reseller',
            'Default' => 'customer',
            'Description' => 'A reseller account can create its own customer accounts',
        ],
    ];
}

/**
 * wpgenie_api calls the provisioning API and returns the decoded JSON.
 * Errors (network, TLS, HTTP >= 300) are thrown with WPGenie's message.
 */
function wpgenie_api(array $params, $method, $path, $body = null, $timeout = 60)
{
    $host = trim($params['serverhostname'] !== '' ? $params['serverhostname'] : $params['serverip']);
    if ($host === '') {
        throw new Exception('Set the WPGenie panel hostname on the server');
    }
    $port = (int) ($params['serverport'] ?? 0);
    // Always HTTPS: the token is a credential and the API must be the
    // panel's (Caddy terminates TLS with a real certificate).
    $url = 'https://' . $host . ($port > 0 && $port !== 443 ? ':' . $port : '') . '/api/v1' . $path;
    $token = trim($params['serveraccesshash'] !== '' ? $params['serveraccesshash'] : $params['serverpassword']);
    if ($token === '') {
        throw new Exception('Set a WPGenie API token as the server password (or access hash)');
    }
    $ch = curl_init($url);
    $headers = [
        'Authorization: Bearer ' . $token,
        'Accept: application/json',
        'User-Agent: WPGenie-WHMCS/1.0',
    ];
    $payload = null;
    if ($body !== null) {
        $payload = json_encode($body);
        $headers[] = 'Content-Type: application/json';
        curl_setopt($ch, CURLOPT_POSTFIELDS, $payload);
    }
    curl_setopt_array($ch, [
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_HTTPHEADER => $headers,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_CONNECTTIMEOUT => 10,
        CURLOPT_TIMEOUT => $timeout,
        CURLOPT_SSL_VERIFYPEER => true,
        CURLOPT_SSL_VERIFYHOST => 2,
        CURLOPT_FOLLOWLOCATION => false,
        CURLOPT_PROTOCOLS => CURLPROTO_HTTPS,
    ]);
    $raw = curl_exec($ch);
    $status = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
    $curlError = curl_error($ch);
    curl_close($ch);

    // The module log masks the token and passwords (last argument).
    $secrets = [$token];
    if (is_array($body)) {
        array_walk_recursive($body, function ($v, $k) use (&$secrets) {
            if ($k === 'password' && is_string($v) && $v !== '') {
                $secrets[] = $v;
            }
        });
    }
    logModuleCall(WPGENIE_MODULE, $method . ' ' . $path, $payload, $raw, null, $secrets);

    if ($raw === false) {
        throw new Exception('Could not reach WPGenie: ' . $curlError);
    }
    $data = $raw === '' ? null : json_decode($raw, true);
    if ($status < 200 || $status >= 300) {
        $msg = is_array($data) && isset($data['error']) ? $data['error'] : ('HTTP ' . $status);
        throw new Exception('WPGenie: ' . $msg);
    }
    return $data;
}

/** wpgenie_account finds the WPGenie account of a WHMCS service. */
function wpgenie_account(array $params)
{
    $list = wpgenie_api($params, 'GET', '/accounts?whmcs_service_id=' . rawurlencode((string) $params['serviceid']));
    if (!is_array($list) || count($list) === 0) {
        throw new Exception('No WPGenie account for service #' . $params['serviceid'] . ' (run Create first)');
    }
    if (count($list) > 1) {
        // WPGenie keeps service IDs unique per billing system; never guess.
        throw new Exception('Several WPGenie accounts have service #' . $params['serviceid'] . '; fix them in WPGenie');
    }
    return $list[0];
}

/** wpgenie_username is the first user's name: the service username, else the client's email. */
function wpgenie_username(array $params)
{
    $name = trim((string) ($params['username'] ?? ''));
    if ($name === '' || ctype_digit($name)) {
        $name = trim((string) ($params['clientsdetails']['email'] ?? ''));
    }
    return substr($name, 0, 64);
}

function wpgenie_TestConnection(array $params)
{
    try {
        wpgenie_api($params, 'GET', '/plans');
        return ['success' => true, 'error' => ''];
    } catch (Exception $e) {
        return ['success' => false, 'error' => $e->getMessage()];
    }
}

function wpgenie_CreateAccount(array $params)
{
    try {
        $client = $params['clientsdetails'] ?? [];
        $name = trim((string) ($client['companyname'] ?? ''));
        if ($name === '') {
            $name = trim(($client['firstname'] ?? '') . ' ' . ($client['lastname'] ?? ''));
        }
        if ($name === '') {
            $name = 'WHMCS service ' . $params['serviceid'];
        }
        $user = ['username' => wpgenie_username($params)];
        // WPGenie requires 12+ characters; with a shorter WHMCS password it
        // generates one, and the client signs in through WHMCS (SSO).
        $password = (string) ($params['password'] ?? '');
        if (strlen($password) >= WPGENIE_MIN_PASSWORD) {
            $user['password'] = $password;
        }
        wpgenie_api($params, 'POST', '/accounts', [
            'name' => mb_substr($name, 0, 100),
            'kind' => $params['configoption2'] !== '' ? $params['configoption2'] : 'customer',
            'plan_id' => trim((string) $params['configoption1']),
            'email' => (string) ($client['email'] ?? ''),
            'whmcs_service_id' => (string) $params['serviceid'],
            'idempotency_key' => 'whmcs-' . $params['serviceid'],
            'user' => $user,
        ]);
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

function wpgenie_SuspendAccount(array $params)
{
    try {
        $a = wpgenie_account($params);
        wpgenie_api($params, 'POST', '/accounts/' . (int) $a['id'] . '/suspend', ['reason' => 'billing']);
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

function wpgenie_UnsuspendAccount(array $params)
{
    try {
        $a = wpgenie_account($params);
        wpgenie_api($params, 'POST', '/accounts/' . (int) $a['id'] . '/unsuspend');
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

function wpgenie_TerminateAccount(array $params)
{
    try {
        $a = wpgenie_account($params);
        // Deleting sites takes a while each; the backups stay in their
        // destinations. Running it again retries what failed.
        wpgenie_api($params, 'POST', '/accounts/' . (int) $a['id'] . '/terminate',
            ['confirm' => (string) $a['id'], 'delete_sites' => true], 600);
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

function wpgenie_ChangePackage(array $params)
{
    try {
        $a = wpgenie_account($params);
        wpgenie_api($params, 'PUT', '/accounts/' . (int) $a['id'], ['plan_id' => trim((string) $params['configoption1'])]);
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

function wpgenie_ChangePassword(array $params)
{
    try {
        $password = (string) ($params['password'] ?? '');
        if (strlen($password) < WPGENIE_MIN_PASSWORD) {
            return 'WPGenie passwords need at least ' . WPGENIE_MIN_PASSWORD . ' characters';
        }
        $a = wpgenie_account($params);
        wpgenie_api($params, 'POST', '/accounts/' . (int) $a['id'] . '/users/' . rawurlencode(wpgenie_username($params)) . '/password',
            ['password' => $password]);
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

/**
 * wpgenie_UsageUpdate copies every account's disk and bandwidth (this
 * month, MB) into WHMCS, for the services on this server.
 */
function wpgenie_UsageUpdate(array $params)
{
    try {
        $report = wpgenie_api($params, 'GET', '/usage', null, 120);
        $mb = 1024 * 1024;
        foreach ((array) $report as $u) {
            $serviceID = (int) ($u['whmcs_service_id'] ?? 0);
            if ($serviceID <= 0) {
                continue;
            }
            Capsule::table('tblhosting')
                ->where('id', $serviceID)
                ->where('server', (int) $params['serverid'])
                ->update([
                    'diskusage' => (int) round($u['disk_bytes'] / $mb),
                    'disklimit' => (int) round($u['disk_limit_bytes'] / $mb),
                    'bwusage' => (int) round($u['bandwidth_bytes'] / $mb),
                    'bwlimit' => (int) round($u['bandwidth_limit_bytes'] / $mb),
                    'lastupdate' => Capsule::raw('now()'),
                ]);
        }
        return 'success';
    } catch (Exception $e) {
        return $e->getMessage();
    }
}

/**
 * wpgenie_ServiceSingleSignOn signs the client in to WPGenie with a
 * one-time link (two minutes, single use). A client with two-factor
 * authentication on still enters their code.
 */
function wpgenie_ServiceSingleSignOn(array $params)
{
    try {
        $a = wpgenie_account($params);
        $out = wpgenie_api($params, 'POST', '/accounts/' . (int) $a['id'] . '/sso', ['username' => wpgenie_username($params)]);
        return ['success' => true, 'redirectTo' => $out['url']];
    } catch (Exception $e) {
        return ['success' => false, 'errorMsg' => $e->getMessage()];
    }
}

function wpgenie_AdminSingleSignOn(array $params)
{
    return wpgenie_ServiceSingleSignOn($params);
}

function wpgenie_ClientArea(array $params)
{
    try {
        $a = wpgenie_account($params);
        $u = wpgenie_api($params, 'GET', '/accounts/' . (int) $a['id'] . '/usage');
        $gb = 1024 * 1024 * 1024;
        $fmt = function ($used, $limit) use ($gb) {
            $text = number_format($used / $gb, 2) . ' GB';
            return $limit > 0 ? $text . ' of ' . number_format($limit / $gb, 2) . ' GB' : $text;
        };
        return [
            'templatefile' => 'templates/clientarea',
            'vars' => [
                'wpgPlan' => $a['plan']['name'] ?? $a['plan_id'],
                'wpgStatus' => $a['effectively_suspended'] ? 'suspended' : $a['status'],
                'wpgSites' => $u['sites'] . ($u['max_sites'] > 0 ? ' of ' . $u['max_sites'] : ''),
                'wpgDisk' => $fmt($u['disk_bytes'], $u['disk_limit_bytes']),
                'wpgBandwidth' => $fmt($u['bandwidth_bytes'], $u['bandwidth_limit_bytes']),
            ],
        ];
    } catch (Exception $e) {
        return [
            'templatefile' => 'templates/error',
            'vars' => ['wpgError' => $e->getMessage()],
        ];
    }
}
