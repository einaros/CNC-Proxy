package discovery

import "testing"

func TestParseRoundTrip(t *testing.T) {
	in := "CARVERA,192.168.1.42,2222,1"
	m, ok := parse(in)
	if !ok {
		t.Fatal("parse failed")
	}
	if m.Name != "CARVERA" || m.IP != "192.168.1.42" || m.Port != 2222 || !m.Busy {
		t.Errorf("parsed = %+v", m)
	}
	if m.format() != in {
		t.Errorf("format = %q, want %q", m.format(), in)
	}
}

func TestParseRejectsShort(t *testing.T) {
	for _, s := range []string{"", "CARVERA", "CARVERA,192.168.1.42", "a,b,notaport,0"} {
		if _, ok := parse(s); ok {
			t.Errorf("parse(%q) unexpectedly succeeded", s)
		}
	}
}

func TestAdvertiseRewritesAddress(t *testing.T) {
	idle := Machine{Name: "CARVERA", IP: "192.168.1.42", Port: 2222, Busy: false}
	adv := Machine{Name: idle.Name + " (proxy)", IP: "192.168.1.50", Port: 2222, Busy: idle.Busy}
	if adv.format() != "CARVERA (proxy),192.168.1.50,2222,0" {
		t.Errorf("advert = %q", adv.format())
	}
}

func TestAdvertisedName(t *testing.T) {
	suffix := &Advertiser{NameSuffix: " (proxy)"}
	if got := suffix.advertisedName("CARVERA"); got != "CARVERA (proxy)" {
		t.Errorf("suffix mode = %q", got)
	}
	override := &Advertiser{Name: "Workshop CNC", NameSuffix: " (proxy)"}
	if got := override.advertisedName("CARVERA"); got != "Workshop CNC" {
		t.Errorf("override mode = %q", got)
	}
}

func TestListenerIgnoresSelf(t *testing.T) {
	l := &Listener{}
	l.SetSelf(Machine{Name: "CARVERA (proxy)", IP: "192.168.1.50", Port: 2222})

	if !l.isSelf(Machine{Name: "CARVERA (proxy)", IP: "192.168.1.50", Port: 2222}) {
		t.Error("own advertisement not filtered")
	}
	// The real machine must never be filtered, even when the operator picks
	// the same advertised name as the real one.
	if l.isSelf(Machine{Name: "CARVERA (proxy)", IP: "192.168.1.42", Port: 2222}) {
		t.Error("real machine filtered because of a name collision")
	}
	if l.isSelf(Machine{Name: "CARVERA", IP: "192.168.1.42", Port: 2222}) {
		t.Error("real machine filtered")
	}
}
