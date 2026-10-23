package convert

import (
	"bytes"
	"testing"
	"testing/iotest"
)

func TestLatin1Reader(t *testing.T) {
	in := make([]byte, 100_000)
	var want []rune
	for i := range in {
		in[i] = byte(i)
		want = append(want, rune(byte(i)))
	}
	r := &latin1Reader{r: iotest.HalfReader(bytes.NewReader(in))}
	if err := iotest.TestReader(r, []byte(string(want))); err != nil {
		t.Fatal(err)
	}
}
