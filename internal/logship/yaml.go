package logship

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A small YAML writer for Vector's configuration: ordered maps, flow lists
// of scalars, and literal blocks for VRL programs. Every string is written
// double-quoted with JSON's escapes (a subset of YAML's), so no value can
// turn into YAML syntax, whatever it holds.

// ymap is a mapping whose keys keep their order.
type ymap []ykv

type ykv struct {
	k string
	v any
}

// yblock is a string written as a literal block (|), line breaks kept.
type yblock string

var yamlKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func marshalYAML(m ymap) ([]byte, error) {
	var b bytes.Buffer
	if err := writeMap(&b, m, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeMap(b *bytes.Buffer, m ymap, indent int) error {
	pad := strings.Repeat(" ", indent)
	for _, kv := range m {
		if !yamlKeyRe.MatchString(kv.k) {
			return fmt.Errorf("yaml: unsafe key %q", kv.k)
		}
		b.WriteString(pad + kv.k + ":")
		switch v := kv.v.(type) {
		case ymap:
			if len(v) == 0 {
				b.WriteString(" {}\n")
				continue
			}
			b.WriteByte('\n')
			if err := writeMap(b, v, indent+2); err != nil {
				return err
			}
		case yblock:
			b.WriteString(" |\n")
			for _, line := range strings.Split(strings.TrimRight(string(v), "\n"), "\n") {
				if line == "" {
					b.WriteByte('\n')
					continue
				}
				b.WriteString(pad + "  " + line + "\n")
			}
		default:
			s, err := yamlScalar(v)
			if err != nil {
				return fmt.Errorf("yaml: %s: %w", kv.k, err)
			}
			b.WriteString(" " + s + "\n")
		}
	}
	return nil
}

func yamlScalar(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return quote(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case []string:
		q := make([]string, len(v))
		for i, s := range v {
			q[i] = quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]", nil
	}
	return "", fmt.Errorf("unsupported value %T", v)
}

// quote is a double-quoted YAML scalar.
func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}
