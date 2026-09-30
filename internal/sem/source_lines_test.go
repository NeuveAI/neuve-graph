package sem

import (
	"reflect"
	"testing"
)

func TestPhysicalSourceLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{name: "empty file", content: "", want: []string{}},
		{name: "newline terminated", content: "first\nsecond\n", want: []string{"first", "second"}},
		{name: "not newline terminated", content: "first\nsecond", want: []string{"first", "second"}},
		{name: "CRLF terminated", content: "first\r\nsecond\r\n", want: []string{"first\r", "second\r"}},
		{name: "one empty line", content: "\n", want: []string{""}},
		{name: "interior blank line", content: "first\n\nthird\n", want: []string{"first", "", "third"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := physicalSourceLines(test.content)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("physicalSourceLines(%q) = %#v, want %#v", test.content, got, test.want)
			}
			if len(got) != sourceLineCount(test.content) {
				t.Fatalf("len(physicalSourceLines(%q)) = %d, sourceLineCount = %d", test.content, len(got), sourceLineCount(test.content))
			}
		})
	}
}
