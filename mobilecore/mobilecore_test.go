package mobilecore

import "testing"

func TestSha256(t *testing.T) {
	if got := Sha256([]byte("savevault")); got != "114790dccea4b6661a521fbd117342f3213db6b4c098a2f3b6138fe2e7848dd5" {
		t.Fatalf("Sha256() = %s", got)
	}
}

func TestBridgeResultExposesErrors(t *testing.T) {
	if got := ManifestHash("/path/that/does/not/exist"); got == "" || got[0] != '{' {
		t.Fatalf("ManifestHash() = %q", got)
	}
}
