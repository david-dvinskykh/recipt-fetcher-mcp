package lidl

import "testing"

// The browser login is stateless: the PKCE verifier and locale must survive a
// round trip through the opaque continuation, because MetaMCP recycles the
// stdio session between the start and resume calls.
func TestContinuationRoundTrip(t *testing.T) {
	cont := encodeContinuation("verifier-123_abc", "PL", "pl")
	v, c, l, ok := decodeContinuation(cont)
	if !ok || v != "verifier-123_abc" || c != "PL" || l != "pl" {
		t.Fatalf("round trip failed: v=%q c=%q l=%q ok=%v", v, c, l, ok)
	}
	for _, bad := range []string{"", "lidl:", "lidl:!!!notbase64", "nope", "lidl:" + "YWJj"} {
		if _, _, _, ok := decodeContinuation(bad); ok {
			t.Errorf("decodeContinuation(%q) unexpectedly ok", bad)
		}
	}
}

func TestParseCode(t *testing.T) {
	cases := map[string]string{
		"ABC123": "ABC123",
		"com.lidlplus.app://callback?code=ABC123": "ABC123",
		"com.lidlplus.app://callback?code=AB&x=1": "AB",
		"code=XYZ&state=1":                        "XYZ",
		"  ABC123  ":                              "ABC123",
	}
	for in, want := range cases {
		if got := parseCode(in); got != want {
			t.Errorf("parseCode(%q) = %q, want %q", in, got, want)
		}
	}
}
