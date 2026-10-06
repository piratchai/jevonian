package jsoncedit

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSetExistingMemberKeepsFormatting(t *testing.T) {
	in := "{\n  // c\n  \"a\": 1,\n  \"b\": \"x\"\n}\n"
	out := ApplyEdits(in, []Edit{{Path: []string{"b"}, Value: "y"}})
	want := "{\n  // c\n  \"a\": 1,\n  \"b\": \"y\"\n}\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestInsertIntoNestedAndEmptyObjects(t *testing.T) {
	in := "{\n  \"env\": {\n    \"A\": \"1\"\n  }\n}\n"
	out := ApplyEdits(in, []Edit{{Path: []string{"env", "B"}, Value: "2"}})
	want := "{\n  \"env\": {\n    \"A\": \"1\",\n    \"B\": \"2\"\n  }\n}\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	empty := ApplyEdits("{\n  \"env\": {}\n}\n", []Edit{{Path: []string{"env", "K"}, Value: "v"}})
	var parsed map[string]map[string]string
	if err := json.Unmarshal([]byte(empty), &parsed); err != nil || parsed["env"]["K"] != "v" {
		t.Fatalf("empty insert:\n%s (%v)", empty, err)
	}
}

func TestRemoveMembersStayValid(t *testing.T) {
	in := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}\n"
	for _, key := range []string{"a", "b", "c"} {
		out := ApplyEdits(in, []Edit{{Path: []string{key}, Remove: true}})
		var parsed map[string]int
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("remove %s:\n%s\n%v", key, out, err)
		}
		if _, ok := parsed[key]; ok || len(parsed) != 2 {
			t.Fatalf("remove %s: %+v", key, parsed)
		}
	}
	// Removing an absent key is a no-op.
	if ApplyEdits(in, []Edit{{Path: []string{"zzz"}, Remove: true}}) != in {
		t.Fatal("absent remove changed text")
	}
}

func TestUnresolvablePathSkipped(t *testing.T) {
	in := `{"a": 1}`
	if ApplyEdits(in, []Edit{{Path: []string{"missing", "x"}, Value: 1}}) != in {
		t.Fatal("intermediate creation must be skipped")
	}
	if ApplyEdits(in, []Edit{{Path: []string{"a", "x"}, Value: 1}}) != in {
		t.Fatal("descending into a non-object must be skipped")
	}
}

func TestPathExistsAndObjectKeys(t *testing.T) {
	text := `{ "env": { "A": "1", /* c */ "B": {"x": [1,2]} }, "s": "/* not a comment */" }`
	if !PathExists(text, []string{"env", "B"}) || PathExists(text, []string{"env", "C"}) {
		t.Fatal("PathExists")
	}
	if got := ObjectKeys(text, []string{"env"}); !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("keys %v", got)
	}
	if ObjectKeys(text, []string{"s"}) != nil || ObjectKeys(text, []string{"nope"}) != nil {
		t.Fatal("non-object keys")
	}
}

func TestUnicodeEscapesInKeys(t *testing.T) {
	text := `{"\u0041": 1}`
	if !PathExists(text, []string{"A"}) {
		t.Fatal("unicode escape key")
	}
}
