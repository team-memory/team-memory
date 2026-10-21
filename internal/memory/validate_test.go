package memory

import "testing"

func TestValidateSourceURI(t *testing.T) {
	for uri, ok := range map[string]bool{
		"":                                true,
		"https://github.com/o/r/issues/1": true,
		"http://wiki.example.com/adr/7":   true,
		"ftp://example.com/x":             false,
		"javascript:alert(1)":             false,
		"https://":                        false,
		"//example.com/x":                 false,
		"https://u:p@example.com":         false,
	} {
		if err := ValidateSourceURI(uri); (err == nil) != ok {
			t.Errorf("%q: got err %v, want ok=%v", uri, err, ok)
		}
	}
}

func TestRoles(t *testing.T) {
	if !RoleMaintainer.AtLeast(RoleWriter) || RoleReader.AtLeast(RoleWriter) || Role("").AtLeast(RoleReader) {
		t.Fatal("role ordering broken")
	}
	if _, err := ParseRole("owner"); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestParseKind(t *testing.T) {
	for _, k := range Kinds {
		if _, err := ParseKind(string(k)); err != nil {
			t.Errorf("%s rejected", k)
		}
	}
	if _, err := ParseKind("rumor"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestCheckText(t *testing.T) {
	if checkText("x", "a\x00b", 0, 10) == nil {
		t.Error("NUL accepted")
	}
	if checkText("x", string([]byte{0xff}), 0, 10) == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if checkText("x", "héllo", 1, 5) != nil {
		t.Error("length should count runes")
	}
}
