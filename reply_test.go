package main

import (
	"bufio"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func parse(s string) (interface{}, error) {
	return readReply(bufio.NewReader(strings.NewReader(s)))
}

func TestReplyTypes(t *testing.T) {
	cases := []struct {
		in   string
		want interface{}
	}{
		{"+OK\r\n", "OK"},
		{":42\r\n", "42"},
		{"-ERR no\r\n", respError("ERR no")},
		{"$3\r\nabc\r\n", "abc"},
		{"$0\r\n\r\n", ""},
		{"$-1\r\n", nil},
		{"*-1\r\n", nil},
		{"*0\r\n", []interface{}{}},
		{"*2\r\n+a\r\n*1\r\n:1\r\n", []interface{}{"a", []interface{}{"1"}}},
		{"+OK\n", "OK"},
	}
	for _, c := range cases {
		got, err := parse(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q: got %#v, want %#v", c.in, got, c.want)
		}
	}
	if e := respError("ERR x"); e.Error() != "ERR x" {
		t.Fatalf("respError.Error() = %q", e.Error())
	}
}

func nested(depth int) string {
	return strings.Repeat("*1\r\n", depth) + "+x\r\n"
}

func TestReplyErrors(t *testing.T) {
	long := "+" + strings.Repeat("a", 5000) + "\r\n"
	twoBulks := "*2\r\n$600000\r\n" + strings.Repeat("a", 600000) + "\r\n" +
		"$600000\r\n" + strings.Repeat("a", 600000) + "\r\n"
	twoArrays := "*2\r\n*3000\r\n" + strings.Repeat(":1\r\n", 3000) + "*3000\r\n"
	cases := []struct {
		name, in, want string
	}{
		{"empty input", "", "EOF"},
		{"empty line", "\r\n", "empty reply line"},
		{"unknown type", "?x\r\n", "unexpected reply"},
		{"bad bulk length", "$x\r\n", "bad length"},
		{"bad array length", "*1x\r\n", "bad length"},
		{"truncated bulk", "$5\r\nab", "EOF"},
		{"truncated array", "*2\r\n+a\r\n", "EOF"},
		{"line too long", long, "line too long"},
		{"bulk too large", "$1048577\r\n", "reply too large"},
		{"bulks too large", twoBulks, "reply too large"},
		{"too many elements", "*4097\r\n", "too many elements"},
		{"elements across arrays", twoArrays, "too many elements"},
		{"too deep", nested(5), "nested too deep"},
		{"far too deep", nested(100000), "nested too deep"},
		{"huge bulk", "$99999999999\r\n", "reply too large"},
		{"huge array", "*99999999\r\n", "too many elements"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(c.in)
			if err == nil || !strings.Contains(err.Error(),
				c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
		})
	}
}

func TestReplyAtLimits(t *testing.T) {
	if _, err := parse(nested(4)); err != nil {
		t.Fatalf("depth 4: %v", err)
	}
	in := fmt.Sprintf("*%d\r\n", maxReplyElems) + strings.Repeat(":1\r\n", maxReplyElems)
	if _, err := parse(in); err != nil {
		t.Fatalf("%d elements: %v", maxReplyElems, err)
	}
	in = fmt.Sprintf("$%d\r\n", maxReplyBytes) + strings.Repeat("a", maxReplyBytes) + "\r\n"
	r := bufio.NewReader(strings.NewReader(in))
	if _, err := readReply(r); err != nil {
		t.Fatalf("%d byte bulk: %v", maxReplyBytes, err)
	}
	line := "+" + strings.Repeat("a", 4000) + "\r\n"
	if _, err := parse(line); err != nil {
		t.Fatalf("4000 byte line: %v", err)
	}
}

func TestReplyLimitsArePerReply(t *testing.T) {
	one := "*3000\r\n" + strings.Repeat(":1\r\n", 3000)
	r := bufio.NewReader(strings.NewReader(one + one))
	for i := 0; i < 2; i++ {
		if _, err := readReply(r); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
}

func TestHostileReplyAllocation(t *testing.T) {
	in := strings.Repeat("*1048576\r\n", 40)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := parse(in)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("hostile reply accepted")
	}
	got := after.TotalAlloc - before.TotalAlloc
	if got > 1<<20 {
		t.Fatalf("%d byte reply allocated %d bytes", len(in), got)
	}
	in = strings.Repeat("*4096\r\n", 40)
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = parse(in)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("hostile reply accepted")
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("%d byte reply allocated %d bytes", len(in), got)
	}
}
