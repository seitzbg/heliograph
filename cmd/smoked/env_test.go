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
