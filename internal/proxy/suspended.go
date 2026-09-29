package proxy

// SuspendedPage is what Caddy answers (503) for every domain of a suspended
// site: static, so a suspended site costs no PHP and needs no shield. It
// goes inside a Caddyfile backtick string, so it must never contain a
// backtick (TestSuspendedPageIsCaddySafe).
const SuspendedPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Temporarily unavailable</title></head>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#333">
<h1 style="font-size:1.4rem">This site is temporarily unavailable</h1>
<p>Please try again later. If this is your site, contact your hosting provider.</p>
</body></html>
`
