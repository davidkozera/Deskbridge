package connfile

import (
	"reflect"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := []Connection{
		{Name: "Office laptop", Host: "192.168.1.20", Port: "47600", Password: "abcd-efgh-jkmn", Width: 1920, Height: 1080, Display: 1},
		{Name: `Den, "the big one"`, Host: "den-pc", Port: "5000", Password: `p,"w` + " d", Display: DisplayAll},
		{Name: "Café", Host: "cafe.local", Port: "47600", Display: DisplayNotSet},
	}
	data := Encode(in)
	if !strings.HasPrefix(string(data), "\xEF\xBB\xBFName,Computer,Port,Password,Resolution,Display\r\n") {
		t.Fatalf("unexpected start: %q", data[:60])
	}
	out, skipped, err := Decode(data)
	if err != nil || skipped != 0 || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %v, skipped %d\n in %+v\nout %+v", err, skipped, in, out)
	}
}

func TestDecodeByHand(t *testing.T) {
	// No header, short rows, a bad port, unsafe characters, a duplicate
	// name, a row without a computer.
	data := "Laptop,10.0.0.5\n" +
		",server2,99999,pw,2560 X 1440,all\n" +
		"Bad|name\x01,host3,80,,nonsense,99\n" +
		"laptop,10.0.0.6\n" +
		"No computer,,47600\n" +
		"\n"
	out, skipped, err := Decode([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []Connection{
		{Name: "Laptop", Host: "10.0.0.5", Port: "47600", Display: DisplayNotSet},
		{Name: "server2", Host: "server2", Port: "47600", Password: "pw", Width: 2560, Height: 1440, Display: DisplayAll},
		{Name: "Badname", Host: "host3", Port: "80", Display: DisplayNotSet},
	}
	if skipped != 2 || !reflect.DeepEqual(out, want) {
		t.Fatalf("skipped %d\n got %+v\nwant %+v", skipped, out, want)
	}
	if _, _, err := Decode([]byte("")); err == nil {
		t.Fatal("an empty file was accepted")
	}
	if _, _, err := Decode([]byte("a,\"b\nc")); err == nil {
		t.Fatal("a broken file was accepted")
	}
	var many strings.Builder
	for i := 0; i < MaxConnections+5; i++ {
		many.WriteString("n" + strings.Repeat("x", i) + ",host\n")
	}
	if out, skipped, _ := Decode([]byte(many.String())); len(out) != MaxConnections || skipped != 5 {
		t.Fatalf("limit: %d read, %d skipped", len(out), skipped)
	}
}
