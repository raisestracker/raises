package httpserver

import (
	"reflect"
	"testing"
)

// The unscoped inbox primitives (List, Get, Notices, Ack, Suppress,
// Unsuppress) read and write any tenant's error groups, so they must stay out
// of the handler Store interface: wiring a handler to one would bypass tenant
// scoping. Keeping them out makes that a compile error; this test fails the
// build if anyone re-adds one.
func TestStoreInterfaceHasNoUnscopedReads(t *testing.T) {
	banned := []string{"List", "Get", "Notices", "Ack", "Suppress", "Unsuppress"}
	iface := reflect.TypeOf((*Store)(nil)).Elem()
	for _, name := range banned {
		if _, ok := iface.MethodByName(name); ok {
			t.Fatalf("httpserver.Store must not expose unscoped %s; use a ForUser wrapper", name)
		}
	}
	required := []string{
		"ListForUser", "GetForUser", "NoticesForUser",
		"AckForUser", "SuppressForUser", "UnsuppressForUser",
	}
	for _, name := range required {
		if _, ok := iface.MethodByName(name); !ok {
			t.Fatalf("httpserver.Store must expose tenant-scoped %s", name)
		}
	}
}
