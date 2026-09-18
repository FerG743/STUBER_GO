package main

import "testing"

func TestApplyOverrides(t *testing.T) {
	mk := func() []TCPStub {
		return []TCPStub{{Name: "a", Port: 8001, Responses: []TCPResponseVariant{{Name: "APROBADA"}, {Name: "DECLINADA"}}}}
	}

	s := mk()
	if err := applyOverrides(s, 8101, "declinada"); err != nil {
		t.Fatal(err)
	}
	if s[0].Port != 8101 || len(s[0].Responses) != 1 || s[0].Responses[0].Name != "DECLINADA" {
		t.Fatalf("port/outcome not applied: %+v", s[0])
	}

	s = mk()
	if err := applyOverrides(s, 0, ""); err != nil || s[0].Port != 8001 || len(s[0].Responses) != 2 {
		t.Fatalf("zero values must be a no-op: %v %+v", err, s[0])
	}

	if err := applyOverrides(mk(), 0, "NOPE"); err == nil {
		t.Fatal("unknown outcome should error")
	}

	two := append(mk(), TCPStub{Name: "b", Port: 9000})
	if err := applyOverrides(two, 8101, ""); err == nil {
		t.Fatal("-port over multiple ports should error")
	}
}
