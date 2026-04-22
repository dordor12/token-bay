//go:build conformance
// +build conformance

// Package conformance runs adversarial prompts against a real `claude -p`
// invocation with the current tool-disabling flag set and asserts zero
// observable side effects. This test is the load-bearing check on the
// seeder role's safety argument.
//
// Implemented progressively in the feature plan for ccbridge.
package conformance

import "testing"

func TestBridgeConformance_Placeholder(t *testing.T) {
	t.Skip("bridge conformance corpus not yet implemented — tracked by internal/ccbridge feature plan")
}
