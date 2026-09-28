package shield

import "strings"

// Class is what we believe the client is, based on its User-Agent and (for
// search engines) a forward-confirmed reverse DNS check.
type Class int

const (
	ClassHuman           Class = iota // no bot signals; a browser as far as we can tell
	ClassVerifiedCrawler              // search engine, verified by FCrDNS
	ClassSpoofedCrawler               // claims to be a search engine but DNS disagrees
	ClassAIBot                        // AI training crawler or AI assistant fetcher
	ClassScript                       // generic HTTP library / empty UA (could be a monitor or webhook)
	ClassAttackTool                   // known vulnerability scanner
)

func (c Class) String() string {
	return [...]string{"human", "verified_crawler", "spoofed_crawler", "ai_bot", "script", "attack_tool"}[c]
}

// IsBot reports whether traffic of this class should be excluded from
// visitor analytics.
func (c Class) IsBot() bool { return c != ClassHuman }

// aiBots are User-Agent substrings (lower-case) of AI crawlers and fetchers.
// Tokens that only exist in robots.txt (Google-Extended, Applebot-Extended)
// are intentionally absent: they never appear in a UA header.
var aiBots = []string{
	"gptbot", "chatgpt-user", "oai-searchbot",
	"claudebot", "claude-web", "claude-user", "claude-searchbot", "anthropic-ai",
	"perplexitybot", "perplexity-user",
	"ccbot", "bytespider", "amazonbot", "meta-externalagent", "meta-externalfetcher",
	"cohere-ai", "cohere-training-data-crawler", "diffbot", "imagesiftbot",
	"omgili", "youbot", "timpibot", "ai2bot", "duckassistbot", "mistralai-user",
	"panscient", "webzio-extended", "img2dataset", "friendlycrawler", "iaskspider",
	"kangaroo bot",
}

var attackTools = []string{
	"sqlmap", "nikto", "nmap", "masscan", "zgrab", "nuclei", "wpscan", "acunetix",
	"netsparker", "dirbuster", "gobuster", "ffuf", "wfuzz", "hydra", "jorgee",
	"fimap", "havij", "w3af", "openvas", "whatweb",
}

var scripts = []string{
	"python-requests", "python-urllib", "aiohttp", "httpx", "go-http-client",
	"curl/", "wget/", "libwww-perl", "java/", "okhttp", "axios/", "node-fetch",
	"scrapy", "httpclient", "guzzlehttp", "headlesschrome", "phantomjs",
}

// searchEngines maps a UA token to the reverse-DNS suffixes its operator
// publishes for verification.
var searchEngines = []struct {
	token    string
	suffixes []string
}{
	{"googlebot", []string{".googlebot.com", ".google.com", ".googleusercontent.com"}},
	{"google-inspectiontool", []string{".googlebot.com", ".google.com"}},
	{"adsbot-google", []string{".googlebot.com", ".google.com"}},
	{"bingbot", []string{".search.msn.com"}},
	{"adidxbot", []string{".search.msn.com"}},
	{"applebot", []string{".applebot.apple.com"}},
	{"yandex", []string{".yandex.ru", ".yandex.net", ".yandex.com"}},
	{"baiduspider", []string{".baidu.com", ".baidu.jp"}},
	{"slurp", []string{".crawl.yahoo.net"}},
}

// classifyUA does the cheap, string-only part of classification. For UAs that
// claim to be a search engine it returns the DNS suffixes that must be
// verified before the client is trusted.
func classifyUA(ua string) (Class, []string) {
	if strings.TrimSpace(ua) == "" {
		return ClassScript, nil
	}
	l := strings.ToLower(ua)
	for _, t := range attackTools {
		if strings.Contains(l, t) {
			return ClassAttackTool, nil
		}
	}
	for _, t := range aiBots {
		if strings.Contains(l, t) {
			return ClassAIBot, nil
		}
	}
	for _, se := range searchEngines {
		if strings.Contains(l, se.token) {
			return ClassVerifiedCrawler, se.suffixes
		}
	}
	for _, t := range scripts {
		if strings.Contains(l, t) {
			return ClassScript, nil
		}
	}
	return ClassHuman, nil
}

// ClassifyUA classifies without DNS verification. Search engine claims are
// taken at face value, which is fine for analytics but never for access
// control — use Shield.Classify there.
func ClassifyUA(ua string) Class {
	c, _ := classifyUA(ua)
	return c
}
