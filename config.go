package rlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// Duration is a time.Duration that is written in YAML and JSON as a string
// such as "30s", "1m30s" or "500ms".
type Duration time.Duration

// String formats d like time.Duration.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalText encodes d as a duration string.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses a duration string such as "1m30s".
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// LoadConfig reads a configuration file on top of DefaultConfig. Files
// ending in .json are parsed as JSON; anything else as YAML. See ParseYAML.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("rlog: read config: %w", err)
	}
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return ParseJSON(data)
	}
	return ParseYAML(data)
}

// ParseYAML decodes a YAML configuration on top of DefaultConfig, so only
// the settings that differ from the defaults need to be given. Keys are the
// snake_case names in the struct tags (max_size_mb, chat_id, ...); levels
// are strings such as "debug", "WARN" or "ERROR+4"; durations are strings
// such as "30s". Unknown keys are an error, to catch typos.
//
// After decoding, every string setting has ${NAME} and ${NAME:-default}
// replaced by the environment variable NAME (or default when it is unset or
// empty), so secrets can stay out of the file:
//
//	telegram:
//	  enabled: true
//	  token: ${TG_TOKEN}
func ParseYAML(data []byte) (Config, error) {
	// WithKnownFields: unknown keys are an error. A Loader (unlike
	// yaml.Load) returns io.EOF for an empty file, which parse turns into
	// the defaults.
	l, err := yaml.NewLoader(bytes.NewReader(data), yaml.WithKnownFields())
	if err != nil {
		return Config{}, fmt.Errorf("rlog: parse yaml config: %w", err)
	}
	return parse("yaml", l.Load)
}

// ParseJSON is ParseYAML for JSON input.
func ParseJSON(data []byte) (Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return parse("json", dec.Decode)
}

// parse runs decode on top of DefaultConfig and expands the environment
// references. Empty input (io.EOF) yields the defaults.
func parse(kind string, decode func(any) error) (Config, error) {
	cfg := DefaultConfig()
	if err := decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("rlog: parse %s config: %w", kind, err)
	}
	expandEnvFields(reflect.ValueOf(&cfg).Elem())
	return cfg, nil
}

// envRef matches ${NAME} and ${NAME:-default}.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv replaces environment references in s; see ParseYAML.
func expandEnv(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if v := os.Getenv(sub[1]); v != "" {
			return v
		}
		return sub[2]
	})
}

// expandEnvFields applies expandEnv to every settable string, and string
// slice element, reachable through structs from v. Expanding after decoding
// (rather than in the raw file) means a value containing YAML or JSON
// syntax characters cannot break the parse.
func expandEnvFields(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				expandEnvFields(f)
			}
		}
	case reflect.String:
		v.SetString(expandEnv(v.String()))
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			for i := range v.Len() {
				expandEnvFields(v.Index(i))
			}
		}
	}
}
