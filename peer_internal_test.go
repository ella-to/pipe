package pipe

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrame_RoundTrip(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		[]byte("a"),
		[]byte("hello world"),
		bytes.Repeat([]byte("x"), 4096),
		bytes.Repeat([]byte("y"), maxDatagramSize),
	}

	for i, want := range cases {
		var buf bytes.Buffer
		if err := writeFrame(&buf, want); err != nil {
			t.Fatalf("case %d: writeFrame: %v", i, err)
		}

		got, err := readFrame(&buf)
		if err != nil {
			t.Fatalf("case %d: readFrame: %v", i, err)
		}

		if len(want) == 0 {
			if len(got) != 0 {
				t.Fatalf("case %d: got %q, want empty", i, got)
			}
			continue
		}

		if !bytes.Equal(got, want) {
			t.Fatalf("case %d: round-trip mismatch (len got=%d want=%d)", i, len(got), len(want))
		}
	}
}

func TestFrame_Sequential(t *testing.T) {
	var buf bytes.Buffer
	frames := [][]byte{[]byte("one"), []byte("two"), []byte("three")}

	for _, f := range frames {
		if err := writeFrame(&buf, f); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}

	for i, want := range frames {
		got, err := readFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: readFrame: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %q, want %q", i, got, want)
		}
	}
}

func TestWriteFrame_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	oversized := make([]byte, maxDatagramSize+1)
	if err := writeFrame(&buf, oversized); !errors.Is(err, ErrDatagramTooLarge) {
		t.Fatalf("writeFrame oversized: got %v, want ErrDatagramTooLarge", err)
	}
}

func TestReadFrame_TooLarge(t *testing.T) {
	// Hand-craft a header advertising a size beyond the cap.
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF}) // ~4 GiB
	if _, err := readFrame(&buf); !errors.Is(err, ErrDatagramTooLarge) {
		t.Fatalf("readFrame oversized: got %v, want ErrDatagramTooLarge", err)
	}
}

func TestReadFrame_TruncatedHeader(t *testing.T) {
	buf := bytes.NewReader([]byte{0x00, 0x01}) // incomplete 4-byte header
	if _, err := readFrame(buf); !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("readFrame truncated header: got %v, want EOF/ErrUnexpectedEOF", err)
	}
}

func TestReadFrame_TruncatedBody(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x00, 0x00, 0x00, 0x08}) // claims 8 bytes
	buf.Write([]byte("123"))                  // only 3 provided
	if _, err := readFrame(&buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("readFrame truncated body: got %v, want ErrUnexpectedEOF", err)
	}
}
