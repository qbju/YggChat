package main

import "testing"

// A member who never set a name is "anon" on the wire; group system lines used
// to say "あきら が anon を副管理者にしました". They now use the short address,
// which the UI resolves to a known name.
func TestGNameOfSkipsAnon(t *testing.T) {
	a := tApp(t, "200::1")
	who := "200:5a42:d631:f4d:f49a:efe7:8bb3:8e66"
	a.store.Contacts[who] = Contact{Addr: who, Nick: "anon"}
	g := Group{ID: "0011223344556677", Members: []string{"200::1", who}}
	if got := a.gNameOf(g, who); got != "200:5a42:…" {
		t.Fatalf("gNameOf = %q", got)
	}
	if got := a.nameOf(who); got != "200:5a42:…" {
		t.Fatalf("nameOf = %q", got)
	}
	a.store.Contacts[who] = Contact{Addr: who, Nick: "ビー"}
	if got := a.gNameOf(g, who); got != "ビー" {
		t.Fatalf("named: %q", got)
	}
}
