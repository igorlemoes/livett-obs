// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package camera

import (
	"bytes"
	"strings"
	"testing"
)

// readBack extracts the codewords from a finished code (trying every mask), checks the error correction of each block and
// returns the text it carries: an independent path from the encoder's own bookkeeping.
func readBack(t *testing.T, modules [][]bool) string {
	t.Helper()
	size := len(modules)
	version := (size - 17) / 4
	for mask := 0; mask < 8; mask++ {
		m := newMatrix(version)
		m.drawFunctionPatterns(version)
		for y := range modules {
			copy(m.dark[y], modules[y])
		}
		m.applyMask(mask)
		var bits []bool
		for right := size - 1; right >= 1; right -= 2 {
			if right == 6 {
				right = 5
			}
			for vert := 0; vert < size; vert++ {
				for j := 0; j < 2; j++ {
					x := right - j
					y := vert
					if (right+1)&2 == 0 {
						y = size - 1 - vert
					}
					if !m.function[y][x] {
						bits = append(bits, m.dark[y][x])
					}
				}
			}
		}
		b := blocksM[version]
		total := b.n1*(b.d1+b.ec) + b.n2*(b.d2+b.ec)
		words := make([]byte, total)
		for i := range words {
			for j := 0; j < 8; j++ {
				words[i] <<= 1
				if bits[i*8+j] {
					words[i] |= 1
				}
			}
		}
		// de-interleave
		blocks := b.n1 + b.n2
		data := make([][]byte, blocks)
		pos := 0
		for i := 0; i < max(b.d1, b.d2); i++ {
			for k := 0; k < blocks; k++ {
				size := b.d1
				if k >= b.n1 {
					size = b.d2
				}
				if i < size {
					data[k] = append(data[k], words[pos])
					pos++
				}
			}
		}
		ec := make([][]byte, blocks)
		for i := 0; i < b.ec; i++ {
			for k := 0; k < blocks; k++ {
				ec[k] = append(ec[k], words[pos])
				pos++
			}
		}
		valid := true
		for k := range data {
			if !bytes.Equal(rsRemainder(data[k], rsGenerator(b.ec)), ec[k]) {
				valid = false
			}
		}
		if !valid {
			continue
		}
		var flat []byte
		for _, d := range data {
			flat = append(flat, d...)
		}
		countBits := 8
		if version >= 10 {
			countBits = 16
		}
		var stream []bool
		for _, w := range flat {
			for j := 7; j >= 0; j-- {
				stream = append(stream, w>>uint(j)&1 == 1)
			}
		}
		read := func(from, n int) int {
			v := 0
			for i := 0; i < n; i++ {
				v <<= 1
				if stream[from+i] {
					v |= 1
				}
			}
			return v
		}
		if read(0, 4) != 0b0100 {
			t.Fatalf("not byte mode")
		}
		n := read(4, countBits)
		out := make([]byte, n)
		for i := range out {
			out[i] = byte(read(4+countBits+8*i, 8))
		}
		return string(out)
	}
	t.Fatalf("no mask gives valid error correction")
	return ""
}

func TestEncodeRoundTripsAcrossVersions(t *testing.T) {
	for _, text := range []string{
		"a",
		"https://192.168.0.10:8766/c/abc?k=def",
		"https://192.168.100.100:8766/c/0123456789abcdef/?k=0123456789abcdef0123456789abcdef",
		strings.Repeat("áéí0123456789", 12), // multi-byte, version 8 or so
		strings.Repeat("x", 213),            // the largest supported
	} {
		modules, err := Encode(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if got := readBack(t, modules); got != text {
			t.Errorf("%d bytes: read back %q", len(text), got)
		}
	}
}

func TestEncodeRefusesWhatDoesNotFit(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", 214)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFinderPatternsAndSize(t *testing.T) {
	modules, err := Encode("https://example.test/")
	if err != nil {
		t.Fatal(err)
	}
	size := len(modules)
	if (size-17)%4 != 0 {
		t.Fatalf("size %d is not a QR size", size)
	}
	for _, corner := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
		x0, y0 := corner[0], corner[1]
		for d := 0; d < 7; d++ {
			if !modules[y0][x0+d] || !modules[y0+6][x0+d] || !modules[y0+d][x0] || !modules[y0+d][x0+6] {
				t.Fatalf("finder at %v has a broken ring", corner)
			}
		}
		if modules[y0+1][x0+1] || !modules[y0+2][x0+2] || !modules[y0+3][x0+3] {
			t.Fatalf("finder at %v has the wrong inside", corner)
		}
	}
}

func TestSVGIsAnImage(t *testing.T) {
	svg, err := SVG("https://example.test/")
	if err != nil || !bytes.HasPrefix(svg, []byte("<svg ")) || !bytes.HasSuffix(svg, []byte("</svg>")) {
		t.Fatalf("%v %.40s", err, svg)
	}
}
