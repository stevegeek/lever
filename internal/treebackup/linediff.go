// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file is adapted from the Go source tree, src/internal/diff/diff.go.
// It returns hunks instead of text, so lever can escape every content line.
//
// Copyright 2009 The Go Authors.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google LLC nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package treebackup

import (
	"bytes"
	"sort"
	"strings"
)

// A pair is a pair of values tracked for both the x and y side of a diff.
// It is typically a pair of line indexes.
type pair struct{ x, y int }

// diffLine is one line of a hunk. Text keeps its trailing newline, so a
// last line without one differs from the same line with one.
type diffLine struct {
	Op   byte // ' ', '-' or '+'
	Text string
}

// hunk is one "@@ -OldStart,OldCount +NewStart,NewCount @@" chunk, with
// 1-based starts (0 for an empty side, as in unified diff).
type hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []diffLine
}

// lineDiff returns an anchored diff of old and new with 3 lines of context,
// or nil when they are equal.
//
// Unix diff implementations typically look for a diff with the smallest
// number of lines inserted and removed, which can in the worst case take
// time quadratic in the number of lines in the texts. This implementation
// looks for a diff with the smallest number of "unique" lines inserted and
// removed, where unique means a line that appears just once in both old and
// new. The unique lines anchor the chosen matching regions, and the
// algorithm runs in O(n log n) time.
func lineDiff(old, new []byte) []hunk {
	if bytes.Equal(old, new) {
		return nil
	}
	x := lines(old)
	y := lines(new)

	var hunks []hunk
	// Loop over matches to consider, expanding each match to include
	// surrounding lines, and then emitting hunks. To avoid setup/teardown
	// cases outside the loop, tgs returns a leading {0,0} and trailing
	// {len(x), len(y)} pair in the sequence of matches.
	var (
		done  pair       // emitted up to x[:done.x] and y[:done.y]
		chunk pair       // start lines of current chunk
		count pair       // number of lines from each side in current chunk
		ctext []diffLine // lines for current chunk
	)
	for _, m := range tgs(x, y) {
		if m.x < done.x {
			// Already handled scanning forward from earlier match.
			continue
		}

		// Expand matching lines as far as possible, establishing that
		// x[start.x:end.x] == y[start.y:end.y]. On the first (or last)
		// iteration the match may (or definitely does) be empty.
		start := m
		for start.x > done.x && start.y > done.y && x[start.x-1] == y[start.y-1] {
			start.x--
			start.y--
		}
		end := m
		for end.x < len(x) && end.y < len(y) && x[end.x] == y[end.y] {
			end.x++
			end.y++
		}

		// Emit the mismatched lines before start into this chunk.
		for _, s := range x[done.x:start.x] {
			ctext = append(ctext, diffLine{'-', s})
			count.x++
		}
		for _, s := range y[done.y:start.y] {
			ctext = append(ctext, diffLine{'+', s})
			count.y++
		}

		// If we're not at EOF and have too few common lines, the chunk
		// includes all the common lines and continues.
		const C = 3 // number of context lines
		if (end.x < len(x) || end.y < len(y)) &&
			(end.x-start.x < C || (len(ctext) > 0 && end.x-start.x < 2*C)) {
			for _, s := range x[start.x:end.x] {
				ctext = append(ctext, diffLine{' ', s})
				count.x++
				count.y++
			}
			done = end
			continue
		}

		// End chunk with common lines for context.
		if len(ctext) > 0 {
			n := min(end.x-start.x, C)
			for _, s := range x[start.x : start.x+n] {
				ctext = append(ctext, diffLine{' ', s})
				count.x++
				count.y++
			}
			done = pair{start.x + n, start.y + n}

			// Convert line numbers to 1-indexed. An empty side shows up
			// as 0,0 not 1,0.
			if count.x > 0 {
				chunk.x++
			}
			if count.y > 0 {
				chunk.y++
			}
			hunks = append(hunks, hunk{
				OldStart: chunk.x, OldCount: count.x,
				NewStart: chunk.y, NewCount: count.y,
				Lines: ctext,
			})
			count.x = 0
			count.y = 0
			ctext = nil
		}

		// If we reached EOF, we're done.
		if end.x >= len(x) && end.y >= len(y) {
			break
		}

		// Otherwise start a new chunk.
		chunk = pair{end.x - C, end.y - C}
		for _, s := range x[chunk.x:end.x] {
			ctext = append(ctext, diffLine{' ', s})
			count.x++
			count.y++
		}
		done = end
	}
	return hunks
}

// lines returns the lines in the file x, including newlines. The last line
// has none when the file does not end in one.
func lines(x []byte) []string {
	l := strings.SplitAfter(string(x), "\n")
	if l[len(l)-1] == "" {
		l = l[:len(l)-1]
	}
	return l
}

// tgs returns the pairs of indexes of the longest common subsequence
// of unique lines in x and y, where a unique line is one that appears
// once in x and once in y.
//
// The longest common subsequence algorithm is as described in
// Thomas G. Szymanski, “A Special Case of the Maximal Common
// Subsequence Problem,” Princeton TR #170 (January 1975),
// available at https://research.swtch.com/tgs170.pdf.
func tgs(x, y []string) []pair {
	// Count the number of times each string appears in a and b.
	// We only care about 0, 1, many, counted as 0, -1, -2
	// for the x side and 0, -4, -8 for the y side.
	// Using negative numbers now lets us distinguish positive line numbers later.
	m := make(map[string]int)
	for _, s := range x {
		if c := m[s]; c > -2 {
			m[s] = c - 1
		}
	}
	for _, s := range y {
		if c := m[s]; c > -8 {
			m[s] = c - 4
		}
	}

	// Now unique strings can be identified by m[s] = -1+-4.
	//
	// Gather the indexes of those strings in x and y, building:
	//	xi[i] = increasing indexes of unique strings in x.
	//	yi[i] = increasing indexes of unique strings in y.
	//	inv[i] = index j such that x[xi[i]] = y[yi[j]].
	var xi, yi, inv []int
	for i, s := range y {
		if m[s] == -1+-4 {
			m[s] = len(yi)
			yi = append(yi, i)
		}
	}
	for i, s := range x {
		if j, ok := m[s]; ok && j >= 0 {
			xi = append(xi, i)
			inv = append(inv, j)
		}
	}

	// Apply Algorithm A from Szymanski's paper.
	// In those terms, A = J = inv and B = [0, n).
	// We add sentinel pairs {0,0}, and {len(x),len(y)}
	// to the returned sequence, to help the processing loop.
	J := inv
	n := len(xi)
	T := make([]int, n)
	L := make([]int, n)
	for i := range T {
		T[i] = n + 1
	}
	for i := 0; i < n; i++ {
		k := sort.Search(n, func(k int) bool {
			return T[k] >= J[i]
		})
		T[k] = J[i]
		L[i] = k + 1
	}
	k := 0
	for _, v := range L {
		if k < v {
			k = v
		}
	}
	seq := make([]pair, 2+k)
	seq[1+k] = pair{len(x), len(y)} // sentinel at end
	lastj := n
	for i := n - 1; i >= 0; i-- {
		if L[i] == k && J[i] < lastj {
			seq[k] = pair{xi[i], yi[J[i]]}
			k--
		}
	}
	seq[0] = pair{0, 0} // sentinel at start
	return seq
}
