package fairshare

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The policy is written to a YAML file, sent over a JSON API, and edited by a
// browser form. All three have to spell every setting the same way.
//
// This caught a real bug: the struct had only yaml tags, so the JSON API used
// Go field names. Go matches JSON keys case-insensitively, which meant
// "weights" and "fairshare" happened to work while "half_life" and
// "backfill_depth" silently did not -- they decoded as zero and were then
// filled back in by WithDefaults, so saving the form appeared to succeed and
// quietly reverted two settings.
//
// Walked by reflection so a field added later is covered without anyone
// remembering to extend this.
func TestEveryFieldSurvivesBothEncodings(t *testing.T) {
	in := Config{
		Enabled:       true,
		Backfill:      true,
		BackfillDepth: 3,
		Weights:       Weights{FairShare: 2.5, Age: 1.25, Size: 0.75},
		Resource:      ResourceWeights{GPU: 30, MemGB: 0.5},
		HalfLife:      72 * time.Hour,
		MaxAge:        24 * time.Hour,
		FavorSmall:    true,
		DefaultShares: 4,
		Shares:        map[string]float64{"alice": 2},
	}

	// Guard against this test going stale: a field left at its zero value
	// cannot detect being dropped.
	rt := reflect.TypeOf(in)
	rv := reflect.ValueOf(in)
	for i := 0; i < rt.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Errorf("this test does not set Config.%s, so it cannot notice "+
				"that field being dropped by an encoder", rt.Field(i).Name)
		}
	}

	for name, round := range map[string]func(Config) (Config, error){
		"json": func(c Config) (Config, error) {
			b, err := json.Marshal(c)
			if err != nil {
				return Config{}, err
			}
			var out Config
			return out, json.Unmarshal(b, &out)
		},
		"yaml": func(c Config) (Config, error) {
			b, err := yaml.Marshal(c)
			if err != nil {
				return Config{}, err
			}
			var out Config
			return out, yaml.Unmarshal(b, &out)
		},
	} {
		out, err := round(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		ov := reflect.ValueOf(out)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i).Name
			want, have := rv.Field(i).Interface(), ov.Field(i).Interface()
			if !reflect.DeepEqual(want, have) {
				t.Errorf("%s dropped Config.%s: wrote %#v, read back %#v",
					name, f, want, have)
			}
		}
	}
}

// Both encodings must use the same key for each setting, or a person reading
// the file and a person reading the API see two different vocabularies.
func TestJSONAndYAMLUseTheSameNames(t *testing.T) {
	rt := reflect.TypeOf(Config{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		j, y := f.Tag.Get("json"), f.Tag.Get("yaml")
		if j == "" || y == "" {
			t.Errorf("Config.%s is missing a %s tag", f.Name,
				map[bool]string{true: "json", false: "yaml"}[j == ""])
			continue
		}
		if j != y {
			t.Errorf("Config.%s is %q in json and %q in yaml", f.Name, j, y)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(Weights{}), reflect.TypeOf(ResourceWeights{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Tag.Get("json") != f.Tag.Get("yaml") {
				t.Errorf("%s.%s: json %q vs yaml %q", typ.Name(), f.Name,
					f.Tag.Get("json"), f.Tag.Get("yaml"))
			}
		}
	}
}

// Every field the CLI and console offer must actually be settable, and every
// settable field should be offered -- a field missing from Fields() is one
// nobody can discover.
func TestFieldsCoversTheSettings(t *testing.T) {
	named := map[string]bool{}
	for _, f := range Fields() {
		named[f.Name] = true
	}
	for _, want := range []string{
		"enabled", "backfill", "backfill-depth",
		"weight-fairshare", "weight-age", "weight-size", "favor-small",
		"half-life", "max-age", "gpu-weight", "mem-gb-weight", "default-shares",
	} {
		if !named[want] {
			t.Errorf("Fields() does not mention %q", want)
		}
	}
	// Shares are per-account and edited separately, so they are correctly
	// absent from the scalar field list.
	if named["shares"] {
		t.Error("Fields() lists shares, which is a per-account map")
	}
}
