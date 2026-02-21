package main

import (
	"slices"
	"testing"
)

func Test_orderRetainingUniqCompact(t *testing.T) {
	actual := slices.CompactFunc([]string{"a", "a", "b", "c", "a", "b", "b"}, orderRetainingUniqCompact[string]())
	expected := []string{"a", "b", "c"}
	if !slices.Equal(expected, actual) {
		t.Fatalf("Expected %v to be %v", expected, actual)
	}
}
