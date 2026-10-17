package main

import (
	"reflect"
	"testing"
)

func TestSplitNamespaces(t *testing.T) {
	cases := map[string][]string{
		"":                 nil,
		" , ":              nil,
		"a":                {"a"},
		"a, b ,a,,c":       {"a", "b", "c"},
		"odoo-prod,odoo-x": {"odoo-prod", "odoo-x"},
	}
	for in, want := range cases {
		if got := splitNamespaces(in); !reflect.DeepEqual(got, want) {
			t.Errorf("splitNamespaces(%q) = %v, want %v", in, got, want)
		}
	}
}
