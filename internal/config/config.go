// Package config reads the deployment INI shared with the rest of this
// codebase's BV-BRC services, for the sra-cache-server binary.
//
// parseINI is ported from Workspace/go/internal/wsconfig/wsconfig.go
// verbatim, including its "null" -> unset convention, so this service's
// config loading behaves identically to the Workspace Go code's. Full
// validation of the keys later phases add (cache root existence, a
// non-empty download-secret, etc.) is explicitly out of scope here -- see
// PLAN-sra-cache-service-implementation.md, Phase 1 -- this package only
// has to load the [SRACache] section generically.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// DefaultSection is the section name used when neither --section nor
// $KB_SERVICE_NAME is set, matching this service's name in the spec/Makefile.
const DefaultSection = "SRACache"

// Config holds the deployment settings this service reads out of its
// section of the deployment INI. It is intentionally small in Phase 1;
// later phases (5, 7, 10) add cache-root/max-cache-size/download-secret/etc.
// fields here as those pieces get built.
type Config struct {
	// Listen is the control-plane (JSONRPC) listen address, e.g. ":7141".
	Listen string

	// DataListen is the data-plane (plain HTTP) listen address, e.g. ":7142".
	DataListen string

	// LogLevel is one of debug/info/warn/error.
	LogLevel string

	// Raw holds every key/value pair read from the section, so a later
	// phase can start consuming a new deploy.cfg key without changing
	// Load's signature.
	Raw map[string]string
}

// Load reads path and extracts the given section. An empty section means
// DefaultSection. Keys absent from the section come back as the zero value
// ("") -- callers apply their own defaults (e.g. from flags) on top, the
// same division of labor ws-download uses with wsconfig.Load.
func Load(path, section string) (*Config, error) {
	if section == "" {
		section = DefaultSection
	}

	raw, err := parseINI(path)
	if err != nil {
		return nil, err
	}
	vals := raw[section]
	if vals == nil {
		return nil, fmt.Errorf("config: no [%s] section in %s", section, path)
	}

	return &Config{
		Listen:     vals["listen"],
		DataListen: vals["data-listen"],
		LogLevel:   vals["log-level"],
		Raw:        vals,
	}, nil
}

// parseINI reads a Config::Simple-style INI: [section] headers, key = value
// pairs, and ';' or '#' comments. It applies two Perl-side conventions: the
// literal string "null" means unset, and an empty value means unset.
//
// Ported verbatim (behavior-for-behavior) from
// Workspace/go/internal/wsconfig/wsconfig.go's parseINI.
func parseINI(path string) (map[string]map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer fh.Close()

	out := map[string]map[string]string{}
	section := ""

	sc := bufio.NewScanner(fh)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("config: %s:%d: expected 'key = value', got %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if val == "null" {
			// Perl maps the literal "null" to undef; deploy.cfg relies on
			// this convention for keys that are intentionally unset.
			val = ""
		}
		if section == "" {
			return nil, fmt.Errorf("config: %s:%d: key %q outside any section", path, lineNo, key)
		}
		out[section][key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return out, nil
}
