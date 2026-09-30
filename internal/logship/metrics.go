package logship

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// Vector's own metrics (its prometheus_exporter sink, Prometheus's text
// format): what the status needs of them.

// sample is one line of the exposition: a metric's labels and value.
type sample struct {
	labels map[string]string
	value  float64
}

// promText parses the text format, keeping only the metrics in want.
func promText(b []byte, want map[string]bool) map[string][]sample {
	out := map[string][]sample{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if !want[name] {
			continue
		}
		rest := line[len(name):]
		labels := map[string]string{}
		if strings.HasPrefix(rest, "{") {
			var ok bool
			if labels, rest, ok = parseLabels(rest[1:]); !ok {
				continue
			}
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out[name] = append(out[name], sample{labels, v})
	}
	return out
}

// parseLabels reads `a="x",b="y\"z"}` and returns what follows the brace.
func parseLabels(s string) (map[string]string, string, bool) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ,")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], true
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || len(s) < eq+2 || s[eq+1] != '"' {
			return nil, "", false
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var val strings.Builder
		i := 0
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i])
				}
				continue
			}
			val.WriteByte(s[i])
		}
		if i >= len(s) {
			return nil, "", false
		}
		labels[key] = val.String()
		s = s[i+1:]
	}
}

// sum adds the values of name's samples whose labels include match.
func sum(m map[string][]sample, name string, match map[string]string) float64 {
	total := 0.0
next:
	for _, smp := range m[name] {
		for k, v := range match {
			if smp.labels[k] != v {
				continue next
			}
		}
		total += smp.value
	}
	return total
}

// vectorMetrics is what the daemon reads from Vector.
type vectorMetrics struct {
	Version string
	// Received is per source (tailed type, or the spool): events and bytes read.
	Received map[string][2]float64
	// Sent and SentBytes: events delivered to the bucket, and the bytes
	// (compressed) it took.
	Sent, SentBytes float64
	Errors          float64
	BufferBytes     float64
}

var wantMetrics = map[string]bool{
	"vector_build_info":                           true,
	"vector_component_received_events_total":      true,
	"vector_component_received_event_bytes_total": true,
	"vector_component_sent_events_total":          true,
	"vector_component_sent_bytes_total":           true,
	"vector_component_errors_total":               true,
	"vector_buffer_size_bytes":                    true,
}

func parseVectorMetrics(b []byte) vectorMetrics {
	m := promText(b, wantMetrics)
	out := vectorMetrics{Received: map[string][2]float64{}}
	for _, smp := range m["vector_build_info"] {
		out.Version = smp.labels["version"]
	}
	for _, t := range Types {
		if t.Collect == collectTailed {
			id := map[string]string{"component_id": sourceID(t.Name)}
			out.Received[t.Name] = [2]float64{sum(m, "vector_component_received_events_total", id),
				sum(m, "vector_component_received_event_bytes_total", id)}
		}
	}
	sink := map[string]string{"component_id": archiveSink}
	out.Sent = sum(m, "vector_component_sent_events_total", sink)
	out.SentBytes = sum(m, "vector_component_sent_bytes_total", sink)
	out.Errors = sum(m, "vector_component_errors_total", sink)
	out.BufferBytes = sum(m, "vector_buffer_size_bytes", sink)
	return out
}
