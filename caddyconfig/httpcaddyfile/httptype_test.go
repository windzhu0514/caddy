package httpcaddyfile

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestMatcherSyntax(t *testing.T) {
	for i, tc := range []struct {
		input       string
		expectWarn  bool
		expectError bool
	}{
		{
			input: `http://localhost
			@debug {
				query showdebug=1
			}
			`,
			expectWarn:  false,
			expectError: false,
		},
		{
			input: `http://localhost
			@debug {
				query bad format
			}
			`,
			expectWarn:  false,
			expectError: true,
		},
		{
			input: `http://localhost
			@debug {
				not {
					path /somepath*
				}
			}
			`,
			expectWarn:  false,
			expectError: false,
		},
		{
			input: `http://localhost
			@debug {
				not path /somepath*
			}
			`,
			expectWarn:  false,
			expectError: false,
		},
	} {

		adapter := caddyfile.Adapter{
			ServerType: ServerType{},
		}

		_, warnings, err := adapter.Adapt([]byte(tc.input), nil)

		if len(warnings) > 0 != tc.expectWarn {
			t.Errorf("Test %d warning expectation failed Expected: %v, got %v", i, tc.expectWarn, warnings)
			continue
		}

		if err != nil != tc.expectError {
			t.Errorf("Test %d error expectation failed Expected: %v, got %s", i, tc.expectError, err)
			continue
		}
	}
}

func TestSpecificity(t *testing.T) {
	for i, tc := range []struct {
		input  string
		expect int
	}{
		{"", 0},
		{"*", 0},
		{"*.*", 1},
		{"{placeholder}", 0},
		{"/{placeholder}", 1},
		{"foo", 3},
		{"example.com", 11},
		{"a.example.com", 13},
		{"*.example.com", 12},
		{"/foo", 4},
		{"/foo*", 4},
		{"{placeholder}.example.com", 12},
		{"{placeholder.example.com", 24},
		{"}.", 2},
		{"}{", 2},
		{"{}", 0},
		{"{{{}}", 1},
	} {
		actual := specificity(tc.input)
		if actual != tc.expect {
			t.Errorf("Test %d (%s): Expected %d but got %d", i, tc.input, tc.expect, actual)
		}
	}
}

func TestGlobalOptions(t *testing.T) {
	for i, tc := range []struct {
		input       string
		expectWarn  bool
		expectError bool
	}{
		{
			input: `
				{
					email test@example.com
				}
				:80
			`,
			expectWarn:  false,
			expectError: false,
		},
		{
			input: `
				{
					admin off
				}
				:80
			`,
			expectWarn:  false,
			expectError: false,
		},
		{
			input: `
				{
					admin 127.0.0.1:2020
				}
				:80
			`,
			expectWarn:  false,
			expectError: false,
		},
		{
			input: `
				{
					admin {
						disabled false
					}
				}
				:80
			`,
			expectWarn:  false,
			expectError: true,
		},
	} {

		adapter := caddyfile.Adapter{
			ServerType: ServerType{},
		}

		_, warnings, err := adapter.Adapt([]byte(tc.input), nil)

		if len(warnings) > 0 != tc.expectWarn {
			t.Errorf("Test %d warning expectation failed Expected: %v, got %v", i, tc.expectWarn, warnings)
			continue
		}

		if err != nil != tc.expectError {
			t.Errorf("Test %d error expectation failed Expected: %v, got %s", i, tc.expectError, err)
			continue
		}
	}
}
