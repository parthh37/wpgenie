package logship

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Vector's configuration, generated from the settings. Verified against
// Vector 0.58's reference (website/cue/reference/components/... at the
// v0.58.0 tag): the file source's remove_after_secs / read_from /
// ignore_older_secs / max_line_bytes, the aws_s3 sink's endpoint, region, force_path_style,
// compression, key_prefix (a template: confined to its literal prefix),
// filename_time_format, filename_append_uuid, filename_extension, batch,
// buffer (disk, at least 256 MiB), acknowledgements and healthcheck, file
// enrichment tables (CSV), internal_metrics and prometheus_exporter.

// Paths inside the shipper's container.
const (
	ctrConfigFile = "/etc/vector/vector.yaml"
	ctrTables     = "/etc/vector/tables"
	ctrData       = "/var/lib/vector"
	ctrSpool      = "/spool"
	ctrCaddy      = "/logs/caddy"
	ctrMail       = "/logs/mail"
	ctrContainers = "/logs/containers"
	// metricsAddr is Vector's Prometheus endpoint, on the container's own
	// loopback: the daemon reads it with docker exec, nothing else can.
	metricsAddr = "127.0.0.1:9598"
	// archiveSink is the S3 sink's component ID (its metrics' label).
	archiveSink = "archive"
	// diskBufferBytes is the S3 sink's disk buffer: Vector's minimum.
	// Logs that wait longer wait in the spool and the files themselves.
	diskBufferBytes = 268435488
	// spoolRemoveAfter: a spool file is deleted this long after Vector
	// read it to the end (and put it in its disk buffer).
	spoolRemoveAfter = 10
)

// serverRe: a server's name in object keys (the panel, or a node's ID).
var serverRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// vectorInput is what the configuration depends on.
type vectorInput struct {
	Settings Settings
	Server   string
	// Tailed are the tailed types that are on and available here.
	Tailed []string
	// AccessLog is the access log's file name (in its directory).
	AccessLog string
}

// sourceID is the file source reading a tailed type (its metrics' label).
func sourceID(typ string) string { return "in_" + typ }

// spoolSource reads every spooled and exported type.
const spoolSource = "in_spool"

var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

// vectorConfig renders vector.yaml.
func vectorConfig(in vectorInput) ([]byte, error) {
	set := in.Settings
	d := set.Destination
	if !serverRe.MatchString(in.Server) {
		return nil, fmt.Errorf("server name %q can't be part of object keys", in.Server)
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	if slices.Contains(in.Tailed, TypeAccess) && !fileNameRe.MatchString(in.AccessLog) {
		return nil, fmt.Errorf("access log name %q", in.AccessLog)
	}
	server := quote(in.Server)

	tables := ymap{{"sites", ymap{
		{"type", "file"},
		{"file", ymap{{"path", ctrTables + "/sites.csv"}, {"encoding", ymap{{"type", "csv"}}}}},
		{"schema", ymap{{"host", "string"}, {"site", "string"}}},
	}}}
	sources := ymap{}
	transforms := ymap{}
	inputs := []string{}
	for _, typ := range in.Tailed {
		var src ymap
		var vrl string
		switch typ {
		case TypeAccess:
			// A new file (Caddy rotated) is read from its start; at the
			// shipper's first start the existing one from its end.
			src = ymap{{"type", "file"}, {"include", []string{ctrCaddy + "/" + in.AccessLog}}, {"read_from", "end"},
				{"max_line_bytes", maxShippedLine}}
			vrl = `ts = .timestamp
line = string!(.message)
. = object(parse_json(line) ?? null) ?? {"message": line}
.timestamp = ts
.log_type = "access"
.server = ` + server + `
host = downcase(string(.request.host) ?? "")
host = replace(host, r':\d+$', "")
if host != "" {
  row, err = get_enrichment_table_record("sites", {"host": host})
  if err == null {
    .site = row.site
  }
}`
		case TypeMail:
			src = ymap{{"type", "file"}, {"include", []string{ctrMail + "/*.log"}}, {"read_from", "end"}}
			vrl = `ts = .timestamp
file = replace(string(.file) ?? "", "` + ctrMail + `/", "")
. = {"message": .message, "file": file, "timestamp": ts, "log_type": "mail", "server": ` + server + `}`
		case TypeContainers:
			// Every container's log is read; only WPGenie's (in the table
			// the daemon keeps) ship.
			tables = append(tables, ykv{"containers", ymap{
				{"type", "file"},
				{"file", ymap{{"path", ctrTables + "/containers.csv"}, {"encoding", ymap{{"type", "csv"}}}}},
				{"schema", ymap{{"id", "string"}, {"name", "string"}}},
			}})
			src = ymap{{"type", "file"}, {"include", []string{ctrContainers + "/*/*-json.log"}}, {"read_from", "end"},
				{"ignore_older_secs", 86400}}
			vrl = `ts = .timestamp
m = parse_regex(string(.file) ?? "", r'/(?P<id>[0-9a-f]{64})-json\.log$') ?? {}
id = string(m.id) ?? ""
if id == "" {
  abort
}
row, err = get_enrichment_table_record("containers", {"id": id})
if err != null {
  abort
}
entry = object(parse_json(string!(.message)) ?? null) ?? {}
. = {"container": row.name, "stream": entry.stream, "time": entry.time, "message": replace(string(entry.log) ?? "", r'\n$', ""), "timestamp": ts, "log_type": "containers", "server": ` + server + `}`
		default:
			return nil, fmt.Errorf("%s isn't a tailed log", typ)
		}
		sources = append(sources, ykv{sourceID(typ), src})
		transforms = append(transforms, ykv{"t_" + typ, remap(sourceID(typ), vrl)})
		inputs = append(inputs, "t_"+typ)
	}

	// The spool: whole files only (the daemon renames them in when
	// they're complete), deleted once read (they're then in the disk
	// buffer). Each starts with a line of its own that makes it unique to
	// Vector's fingerprint (a checksum of the first line) and is dropped.
	var spooled []string
	for _, t := range Types {
		if t.Collect != collectTailed {
			spooled = append(spooled, quote(t.Name))
		}
	}
	sources = append(sources,
		ykv{spoolSource, ymap{{"type", "file"}, {"include", []string{ctrSpool + "/*/*.jsonl"}}, {"read_from", "beginning"},
			{"remove_after_secs", spoolRemoveAfter}, {"max_line_bytes", maxShippedLine}}},
		ykv{"in_internal", ymap{{"type", "internal_metrics"}, {"scrape_interval_secs", 15}}})
	transforms = append(transforms, ykv{"t_spool", remap(spoolSource, `ts = .timestamp
m = parse_regex(string(.file) ?? "", r'^`+ctrSpool+`/(?P<kind>[a-z_]+)/') ?? {}
kind = string(m.kind) ?? ""
if !includes([`+strings.Join(spooled, ", ")+`], kind) {
  abort
}
. = object(parse_json(string!(.message)) ?? null) ?? {}
if length(.) == 0 || exists(.wpgenie_spool) {
  abort
}
.timestamp = ts
.log_type = kind
.server = `+server)})
	inputs = append(inputs, "t_spool")

	ext := "log.gz"
	if set.Compression == "zstd" {
		ext = "log.zst"
	}
	sink := ymap{
		{"type", "aws_s3"},
		{"inputs", inputs},
		{"endpoint", d.Endpoint},
		{"region", d.region()},
		{"bucket", d.Bucket},
		{"force_path_style", d.PathStyle},
		// <prefix><server>/<type>/YYYY/MM/DD/HH-<uuid>.log.gz
		{"key_prefix", d.Prefix + in.Server + "/{{ log_type }}/%Y/%m/%d/"},
		{"filename_time_format", "%H"},
		{"filename_append_uuid", true},
		{"filename_extension", ext},
		{"timezone", "UTC"},
		{"compression", set.Compression},
		{"encoding", ymap{{"codec", "json"}}},
		{"framing", ymap{{"method", "newline_delimited"}}},
		{"batch", ymap{{"max_bytes", set.BatchMaxMB * 1_000_000}, {"timeout_secs", set.BatchMaxSeconds}}},
		// Outages: batches wait on disk (a crash loses nothing that was
		// read), then the spool and the files wait to be read.
		{"buffer", ymap{{"type", "disk"}, {"max_size", diskBufferBytes}, {"when_full", "block"}}},
		{"acknowledgements", ymap{{"enabled", true}}},
		// Credentials may be allowed to write objects only; the panel's
		// "Test connection" checks what's needed.
		{"healthcheck", ymap{{"enabled", false}}},
	}
	return marshalYAML(ymap{
		{"data_dir", ctrData},
		{"enrichment_tables", tables},
		{"sources", sources},
		{"transforms", transforms},
		{"sinks", ymap{
			{archiveSink, sink},
			{"metrics", ymap{{"type", "prometheus_exporter"}, {"inputs", []string{"in_internal"}}, {"address", metricsAddr}}},
		}},
	})
}

// remap is a remap transform whose events are dropped when the program
// aborts or fails (they would reach the sink without a type).
func remap(input, vrl string) ymap {
	return ymap{{"type", "remap"}, {"inputs", []string{input}}, {"drop_on_abort", true}, {"drop_on_error", true},
		{"source", yblock(vrl)}}
}
