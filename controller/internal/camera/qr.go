// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package camera

import (
	"errors"
	"fmt"
	"strings"
)

// A small QR Code generator (ISO/IEC 18004): byte mode, error correction level M, versions 1 to 10 (up to 213 bytes).
// It exists so the panel can show the pairing link as a code without any dependency; the link is a short URL.

// level M block layout per version: EC codewords per block, then (blocks, data codewords) of group 1 and group 2.
var blocksM = [11]struct{ ec, n1, d1, n2, d2 int }{
	{}, {10, 1, 16, 0, 0}, {16, 1, 28, 0, 0}, {26, 1, 44, 0, 0}, {18, 2, 32, 0, 0}, {24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0}, {18, 4, 31, 0, 0}, {22, 2, 38, 2, 39}, {22, 3, 36, 2, 37}, {26, 4, 43, 1, 44},
}

var alignmentCenters = [11][]int{nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}}

// ---- Reed-Solomon over GF(256), polynomial 0x11D

var gfExp [512]byte
var gfLog [256]int

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

// rsGenerator is the product of (x - α^i) for i < degree, highest term first without the leading 1.
func rsGenerator(degree int) []byte {
	poly := make([]byte, degree)
	poly[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range poly {
			poly[j] = gfMul(poly[j], root)
			if j+1 < len(poly) {
				poly[j] ^= poly[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return poly
}

func rsRemainder(data, generator []byte) []byte {
	out := make([]byte, len(generator))
	for _, b := range data {
		factor := b ^ out[0]
		copy(out, out[1:])
		out[len(out)-1] = 0
		for i, g := range generator {
			out[i] ^= gfMul(g, factor)
		}
	}
	return out
}

// ---- encoding

func dataCapacity(version int) int {
	b := blocksM[version]
	return b.n1*b.d1 + b.n2*b.d2
}

type bitBuffer struct{ bits []bool }

func (b *bitBuffer) add(value uint, count int) {
	for i := count - 1; i >= 0; i-- {
		b.bits = append(b.bits, value>>uint(i)&1 == 1)
	}
}

// codewords returns the data codewords (with padding) of text for the version, or an error when it does not fit.
func codewords(text []byte, version int) ([]byte, error) {
	countBits := 8
	if version >= 10 {
		countBits = 16
	}
	capacity := dataCapacity(version) * 8
	if 4+countBits+8*len(text) > capacity {
		return nil, errors.New("text does not fit")
	}
	var b bitBuffer
	b.add(0b0100, 4)
	b.add(uint(len(text)), countBits)
	for _, c := range text {
		b.add(uint(c), 8)
	}
	for n := 0; n < 4 && len(b.bits) < capacity; n++ {
		b.bits = append(b.bits, false)
	}
	for len(b.bits)%8 != 0 {
		b.bits = append(b.bits, false)
	}
	out := make([]byte, 0, capacity/8)
	for i := 0; i < len(b.bits); i += 8 {
		var v byte
		for j := 0; j < 8; j++ {
			v <<= 1
			if b.bits[i+j] {
				v |= 1
			}
		}
		out = append(out, v)
	}
	for pad := byte(0xEC); len(out) < capacity/8; pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out, nil
}

// interleave splits the data into blocks, adds the error correction of each and interleaves everything.
func interleave(data []byte, version int) []byte {
	b := blocksM[version]
	generator := rsGenerator(b.ec)
	var dataBlocks, ecBlocks [][]byte
	pos := 0
	for i := 0; i < b.n1+b.n2; i++ {
		size := b.d1
		if i >= b.n1 {
			size = b.d2
		}
		block := data[pos : pos+size]
		pos += size
		dataBlocks = append(dataBlocks, block)
		ecBlocks = append(ecBlocks, rsRemainder(block, generator))
	}
	var out []byte
	for i := 0; i < b.d2 || i < b.d1; i++ {
		for _, block := range dataBlocks {
			if i < len(block) {
				out = append(out, block[i])
			}
		}
	}
	for i := 0; i < b.ec; i++ {
		for _, block := range ecBlocks {
			out = append(out, block[i])
		}
	}
	return out
}

// ---- matrix

type matrix struct {
	size     int
	dark     [][]bool
	function [][]bool // modules that are not data (finders, timing, format...)
}

func newMatrix(version int) *matrix {
	size := 17 + 4*version
	m := &matrix{size: size, dark: make([][]bool, size), function: make([][]bool, size)}
	for i := range m.dark {
		m.dark[i] = make([]bool, size)
		m.function[i] = make([]bool, size)
	}
	return m
}

func (m *matrix) set(x, y int, dark bool) {
	m.dark[y][x] = dark
	m.function[y][x] = true
}

func (m *matrix) finder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= m.size || y >= m.size {
				continue
			}
			dist := max(abs(dx), abs(dy))
			m.set(x, y, dist != 2 && dist != 4)
		}
	}
}

func (m *matrix) alignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			m.set(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (m *matrix) drawFunctionPatterns(version int) {
	for i := 0; i < m.size; i++ {
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}
	m.finder(3, 3)
	m.finder(m.size-4, 3)
	m.finder(3, m.size-4)
	centers := alignmentCenters[version]
	for i, cy := range centers {
		for j, cx := range centers {
			if (i == 0 && j == 0) || (i == 0 && j == len(centers)-1) || (i == len(centers)-1 && j == 0) {
				continue // the three finders
			}
			m.alignment(cx, cy)
		}
	}
	m.drawFormat(0) // reserves the format areas; the real mask is written later
	if version >= 7 {
		m.drawVersion(version)
	}
}

func bit(value, i int) bool { return value>>uint(i)&1 == 1 }

func (m *matrix) drawFormat(mask int) {
	data := 0<<3 | mask // error correction level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	for i := 0; i <= 5; i++ {
		m.set(8, i, bit(bits, i))
	}
	m.set(8, 7, bit(bits, 6))
	m.set(8, 8, bit(bits, 7))
	m.set(7, 8, bit(bits, 8))
	for i := 9; i < 15; i++ {
		m.set(14-i, 8, bit(bits, i))
	}
	for i := 0; i < 8; i++ {
		m.set(m.size-1-i, 8, bit(bits, i))
	}
	for i := 8; i < 15; i++ {
		m.set(8, m.size-15+i, bit(bits, i))
	}
	m.set(8, m.size-8, true)
}

func (m *matrix) drawVersion(version int) {
	rem := version
	for i := 0; i < 12; i++ {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	bits := version<<12 | rem
	for i := 0; i < 18; i++ {
		a, b := m.size-11+i%3, i/3
		m.set(a, b, bit(bits, i))
		m.set(b, a, bit(bits, i))
	}
}

func (m *matrix) drawCodewords(data []byte) {
	i := 0
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < m.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = m.size - 1 - vert
				}
				if !m.function[y][x] && i < len(data)*8 {
					m.dark[y][x] = bit(int(data[i>>3]), 7-i&7)
					i++
				}
			}
		}
	}
}

func maskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (x/3+y/2)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	}
	return ((x+y)%2+x*y%3)%2 == 0
}

func (m *matrix) applyMask(mask int) {
	for y := 0; y < m.size; y++ {
		for x := 0; x < m.size; x++ {
			if !m.function[y][x] && maskBit(mask, x, y) {
				m.dark[y][x] = !m.dark[y][x]
			}
		}
	}
}

func (m *matrix) penalty() int {
	total := 0
	runs := func(get func(i, j int) bool) {
		for i := 0; i < m.size; i++ {
			run := 1
			for j := 1; j <= m.size; j++ {
				if j < m.size && get(i, j) == get(i, j-1) {
					run++
					continue
				}
				if run >= 5 {
					total += 3 + run - 5
				}
				run = 1
			}
		}
	}
	runs(func(i, j int) bool { return m.dark[i][j] })
	runs(func(i, j int) bool { return m.dark[j][i] })
	for y := 0; y+1 < m.size; y++ {
		for x := 0; x+1 < m.size; x++ {
			c := m.dark[y][x]
			if c == m.dark[y][x+1] && c == m.dark[y+1][x] && c == m.dark[y+1][x+1] {
				total += 3
			}
		}
	}
	pattern := []bool{true, false, true, true, true, false, true}
	finderLike := func(get func(k int) bool) int {
		count := 0
		for k := 0; k+11 <= m.size; k++ {
			a, b := true, true
			for i := 0; i < 7; i++ {
				if get(k+i) != pattern[i] {
					a = false
				}
				if get(k+4+i) != pattern[i] {
					b = false
				}
			}
			for i := 0; i < 4; i++ {
				if get(k + 7 + i) {
					a = false
				}
				if get(k + i) {
					b = false
				}
			}
			if a {
				count++
			}
			if b {
				count++
			}
		}
		return count
	}
	for i := 0; i < m.size; i++ {
		total += 40 * finderLike(func(k int) bool { return m.dark[i][k] })
		total += 40 * finderLike(func(k int) bool { return m.dark[k][i] })
	}
	darkCount := 0
	for _, row := range m.dark {
		for _, d := range row {
			if d {
				darkCount++
			}
		}
	}
	cells := m.size * m.size
	diff := abs(darkCount*20 - cells*10)
	total += (diff + cells - 1) / cells * 10 // each 5% away from half counts 10
	return total
}

// Encode returns the QR modules of text (true = dark), without the quiet zone.
func Encode(text string) ([][]bool, error) {
	raw := []byte(text)
	version := 0
	for v := 1; v <= 10; v++ {
		if _, err := codewords(raw, v); err == nil {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, errors.New("o texto é grande demais para o QR code")
	}
	data, _ := codewords(raw, version)
	all := interleave(data, version)
	m := newMatrix(version)
	m.drawFunctionPatterns(version)
	m.drawCodewords(all)
	best, bestScore := 0, -1
	for mask := 0; mask < 8; mask++ {
		m.applyMask(mask)
		m.drawFormat(mask)
		if score := m.penalty(); bestScore < 0 || score < bestScore {
			best, bestScore = mask, score
		}
		m.applyMask(mask)
	}
	m.applyMask(best)
	m.drawFormat(best)
	return m.dark, nil
}

// SVG renders the code with a quiet zone of four modules, ready for an <img>.
func SVG(text string) ([]byte, error) {
	modules, err := Encode(text)
	if err != nil {
		return nil, err
	}
	const quiet = 4
	size := len(modules) + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, size, size, size, size)
	for y, row := range modules {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String()), nil
}
