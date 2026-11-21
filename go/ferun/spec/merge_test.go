// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package spec

import "testing"

func TestMergePatch(t *testing.T) {
	// RFC 7396, appendix A.
	cases := []struct{ doc, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`["a","b"]`, `["c","d"]`, `["c","d"]`},
		{`{"a":"b"}`, `["c"]`, `["c"]`},
		{`{"a":"foo"}`, `null`, `null`},
		{`{"a":"foo"}`, `"bar"`, `"bar"`},
		{`{"e":null}`, `{"a":1}`, `{"a":1,"e":null}`},
		{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
		{``, `{"a":1}`, `{"a":1}`},
		{`{"n":12345678901234567890}`, `{"m":1.50}`, `{"m":1.50,"n":12345678901234567890}`},
	}
	for _, c := range cases {
		got, err := MergePatch([]byte(c.doc), []byte(c.patch))
		if err != nil || string(got) != c.want {
			t.Errorf("MergePatch(%s, %s) = %s, %v; want %s", c.doc, c.patch, got, err, c.want)
		}
	}
	for _, c := range [][2]string{{`{`, `{}`}, {`{}`, `{`}, {`{}`, `{} {}`}, {`{}`, ``}} {
		if _, err := MergePatch([]byte(c[0]), []byte(c[1])); err == nil {
			t.Errorf("MergePatch(%s, %s): no error", c[0], c[1])
		}
	}
}
