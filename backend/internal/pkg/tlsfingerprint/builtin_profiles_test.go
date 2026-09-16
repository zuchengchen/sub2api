package tlsfingerprint

import "testing"

func TestBuiltinProfileNodejs24HasCipherSuites(t *testing.T) {
	p := BuiltinProfile("nodejs24")
	if p == nil {
		t.Fatal("nodejs24 profile missing")
	}
	if len(p.CipherSuites) == 0 || p.Name == "" {
		t.Fatalf("incomplete nodejs24 profile: %+v", p)
	}
	if BuiltinProfile("unknown") != nil {
		t.Fatal("unknown profile must be nil")
	}
}
