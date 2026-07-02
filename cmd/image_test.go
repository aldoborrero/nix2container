package cmd

import (
	"reflect"
	"testing"
)

func TestMergeBaseEnv(t *testing.T) {
	for _, tc := range []struct {
		name     string
		base     []string
		new      []string
		expected []string
	}{
		{
			name:     "base env carries through under new env",
			base:     []string{"PATH=/base/bin", "LD_LIBRARY_PATH=/base/lib"},
			new:      []string{"FOO=bar"},
			expected: []string{"PATH=/base/bin", "LD_LIBRARY_PATH=/base/lib", "FOO=bar"},
		},
		{
			name:     "new env wins on conflicting key and comes last",
			base:     []string{"PATH=/base/bin", "LANG=C"},
			new:      []string{"PATH=/new/bin"},
			expected: []string{"LANG=C", "PATH=/new/bin"},
		},
		{
			name:     "no base env",
			base:     nil,
			new:      []string{"FOO=bar"},
			expected: []string{"FOO=bar"},
		},
		{
			name:     "no new env",
			base:     []string{"FOO=bar"},
			new:      nil,
			expected: []string{"FOO=bar"},
		},
		{
			name:     "empty value still counts as set",
			base:     []string{"FOO=base"},
			new:      []string{"FOO="},
			expected: []string{"FOO="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeBaseEnv(tc.base, tc.new)
			if !reflect.DeepEqual(got, tc.expected) {
				t.Fatalf("mergeBaseEnv(%v, %v) = %v, want %v", tc.base, tc.new, got, tc.expected)
			}
		})
	}
}
