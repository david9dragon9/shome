package qos

import "testing"

// The distinction this package turns on: a limit nobody set is unlimited, and
// a limit set to zero forbids everything. Collapsing those -- which the first
// version did, using 0 for both -- means an admin who writes
// "max-running-gpus: 0" grants unlimited accelerators.
func TestZeroIsNotUnlimited(t *testing.T) {
	def := Limits{MaxRunningGPUs: ptr(4)}
	user := Limits{MaxRunningGPUs: ptr(0)}

	if e := Resolve(def, Limits{}); e.MaxRunningGPUs != 4 {
		t.Errorf("inherited default = %d, want 4", e.MaxRunningGPUs)
	}
	if e := Resolve(def, user); e.MaxRunningGPUs != 0 {
		t.Errorf("explicit zero = %d, want 0 (none allowed)", e.MaxRunningGPUs)
	}
	if e := Resolve(Limits{}, Limits{}); e.MaxRunningGPUs != Unlimited {
		t.Errorf("unset at both layers = %d, want Unlimited", e.MaxRunningGPUs)
	}
}

func TestOverrideReplacesOnlyItsOwnField(t *testing.T) {
	def := Defaults()
	e := Resolve(def, Limits{MaxRunningJobs: ptr(2)})
	if e.MaxRunningJobs != 2 {
		t.Errorf("MaxRunningJobs = %d, want 2", e.MaxRunningJobs)
	}
	if e.MaxRunningCPUs != *def.MaxRunningCPUs {
		t.Errorf("an unrelated limit changed: %d", e.MaxRunningCPUs)
	}
}

func TestDefaultsAreBounded(t *testing.T) {
	// "No configuration" must still mean "bounded". A cluster of other
	// people's computers should not be unlimited by omission.
	e := Resolve(Defaults(), Limits{})
	for name, v := range map[string]int{
		"MaxSubmittedJobs": e.MaxSubmittedJobs,
		"MaxRunningJobs":   e.MaxRunningJobs,
		"MaxRunningCPUs":   e.MaxRunningCPUs,
		"MaxRunningGPUs":   e.MaxRunningGPUs,
		"MaxProcsPerJob":   e.MaxProcsPerJob,
	} {
		if v <= 0 {
			t.Errorf("%s defaults to %d; it should be a real limit", name, v)
		}
	}
	if e.MaxWalltime <= 0 {
		t.Error("MaxWalltime has no default; a job could run forever")
	}
}

func TestParseSizeMB(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{"512M", 512, false}, {"8G", 8192, false}, {"2T", 2 << 20, false},
		{"1024", 1024, false}, {"unlimited", Unlimited, false},
		{"0", 0, false}, {"", 0, true}, {"-5G", 0, true}, {"lots", 0, true},
	} {
		got, err := ParseSizeMB(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseSizeMB(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSizeMB(%q): %v", tc.in, err)
		} else if got != tc.want {
			t.Errorf("ParseSizeMB(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSetAndClearRoundTrip(t *testing.T) {
	var l Limits
	f, ok := FieldByName("max-running-gpus")
	if !ok {
		t.Fatal("field not found")
	}
	if err := f.Set(&l, "3"); err != nil {
		t.Fatal(err)
	}
	if l.MaxRunningGPUs == nil || *l.MaxRunningGPUs != 3 {
		t.Fatalf("set produced %v", l.MaxRunningGPUs)
	}
	f.Clear(&l)
	if l.MaxRunningGPUs != nil {
		t.Error("clear left the field set")
	}
	// And "unlimited" is expressible.
	if err := f.Set(&l, "unlimited"); err != nil {
		t.Fatal(err)
	}
	if l.MaxRunningGPUs == nil || *l.MaxRunningGPUs != Unlimited {
		t.Errorf("unlimited produced %v", l.MaxRunningGPUs)
	}
}

// Every field must be settable and clearable by name, or a limit exists that
// the CLI, the file and the console cannot all reach.
func TestEveryFieldIsAddressable(t *testing.T) {
	for _, f := range Fields {
		got, ok := FieldByName(f.Name)
		if !ok {
			t.Errorf("%s is not findable by name", f.Name)
			continue
		}
		var l Limits
		v := "1"
		if f.Unit == "duration" {
			v = "1h"
		}
		if err := got.Set(&l, v); err != nil {
			t.Errorf("%s cannot be set: %v", f.Name, err)
		}
		got.Clear(&l)
	}
}

func TestDisplayDistinguishesNoneFromUnlimited(t *testing.T) {
	if itoa(Unlimited) != "unlimited" {
		t.Errorf("unlimited displays as %q", itoa(Unlimited))
	}
	if itoa(0) == "unlimited" {
		t.Error("zero displays as unlimited, which is the opposite of what it means")
	}
	if mb(Unlimited) != "unlimited" || mb(0) == "unlimited" {
		t.Errorf("size display confuses none with unlimited: %q / %q", mb(Unlimited), mb(0))
	}
}
