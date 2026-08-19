package main

import (
	"os"
	"testing"
)

// envBool backs boolean flags whose default can come from the environment (e.g. -downsample /
// SMOKED_DOWNSAMPLE), so a Compose/K8s deployment can drive them via `environment:` instead of the
// command list. It follows strconv.ParseBool: 1/t/T/TRUE/true/True are true; empty or anything
// unparseable is false.
func TestEnvBool(t *testing.T) {
	const key = "SMOKED_TEST_ENVBOOL"
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"t", true},
		{"0", false},
		{"false", false},
		{"", false},
		{"yes", false}, // ParseBool does not accept yes/on
		{"garbage", false},
	}
	for _, c := range cases {
		t.Setenv(key, c.val)
		if got := envBool(key); got != c.want {
			t.Errorf("envBool(%q) = %v, want %v", c.val, got, c.want)
		}
	}
	os.Unsetenv(key)
	if envBool(key) {
		t.Errorf("envBool(unset) = true, want false")
	}
}

// envBoolOr is envBool with a non-false default: unset/unparseable yields the default, a set value
// parses via strconv.ParseBool. Used for flags that default true (e.g. -absolute-time).
func TestEnvBoolOr(t *testing.T) {
	const key = "SMOKED_TEST_ENVBOOLOR"
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"", true, true},   // unset -> default
		{"", false, false}, // unset -> default
		{"0", true, false}, // explicit override of a true default
		{"false", true, false},
		{"1", false, true}, // explicit override of a false default
		{"true", false, true},
		{"garbage", true, true}, // unparseable -> default
		{"garbage", false, false},
	}
	for _, c := range cases {
		if c.val == "" {
			os.Unsetenv(key)
		} else {
			t.Setenv(key, c.val)
		}
		if got := envBoolOr(key, c.def); got != c.want {
			t.Errorf("envBoolOr(%q, def=%v) = %v, want %v", c.val, c.def, got, c.want)
		}
	}
	os.Unsetenv(key)
}
