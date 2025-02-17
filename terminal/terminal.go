// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package terminal

import (
	"fmt"
	"io"
	"os"
	"sync"
)

func max(i, j int) int {
	if i > j {
		return i
	}
	return j
}

func min(i, j int) int {
	if i < j {
		return i
	}
	return j
}

// historyIdxValue returns an index into a valid range of history
func historyIdxValue(idx int, history [][]byte) int {
	out := idx
	out = min(len(history), out)
	out = max(0, out)
	return out
}

// EscapeCodes contains escape sequences that can be written to the terminal in
// order to achieve different styles of text.
type EscapeCodes struct {
	// Foreground colors
	Black, Red, Green, Yellow, Blue, Magenta, Cyan, White []byte

	// Reset all attributes
	Reset []byte
}

var vt100EscapeCodes = EscapeCodes{
	Black:   []byte{KeyEscape, '[', '3', '0', 'm'},
	Red:     []byte{KeyEscape, '[', '3', '1', 'm'},
	Green:   []byte{KeyEscape, '[', '3', '2', 'm'},
	Yellow:  []byte{KeyEscape, '[', '3', '3', 'm'},
	Blue:    []byte{KeyEscape, '[', '3', '4', 'm'},
	Magenta: []byte{KeyEscape, '[', '3', '5', 'm'},
	Cyan:    []byte{KeyEscape, '[', '3', '6', 'm'},
	White:   []byte{KeyEscape, '[', '3', '7', 'm'},

	Reset: []byte{KeyEscape, '[', '0', 'm'},
}

// Terminal contains the state for running a VT100 terminal that is capable of
// reading lines of input.
type Terminal struct {
	// AutoCompleteCallback, if non-null, is called for each keypress
	// with the full input line and the current position of the cursor.
	// If it returns a nil newLine, the key press is processed normally.
	// Otherwise it returns a replacement line and the new cursor position.
	AutoCompleteCallback func(line []byte, pos, key int) (newLine []byte, newPos int)

	// Escape contains a pointer to the escape codes for this terminal.
	// It's always a valid pointer, although the escape codes themselves
	// may be empty if the terminal doesn't support them.
	Escape *EscapeCodes

	// lock protects the terminal and the state in this object from
	// concurrent processing of a key press and a Write() call.
	lock sync.Mutex

	c      io.ReadWriter
	prompt string

	// line is the current line being entered.
	line []byte
	// history is a buffer of previously entered lines
	history [][]byte
	// index into the history buffer (for use in the handleKey(KeyUp) function)
	historyIdx int
	// pos is the logical position of the cursor in line
	pos int
	// echo is true if local echo is enabled
	echo bool

	// cursorX contains the current X value of the cursor where the left
	// edge is 0. cursorY contains the row number where the first row of
	// the current line is 0.
	cursorX, cursorY int
	// maxLine is the greatest value of cursorY so far.
	maxLine int

	termWidth, termHeight int

	// outBuf contains the terminal data to be sent.
	outBuf []byte
	// remainder contains the remainder of any partial key sequences after
	// a read. It aliases into inBuf.
	remainder []byte
	inBuf     [256]byte
}

// NewTerminal runs a VT100 terminal on the given ReadWriter. If the ReadWriter is
// a local terminal, that terminal must first have been put into raw mode.
// prompt is a string that is written at the start of each input line (i.e.
// "> ").
func NewTerminal(c io.ReadWriter, prompt string, echo bool) *Terminal {
	return &Terminal{
		Escape:     &vt100EscapeCodes,
		c:          c,
		prompt:     prompt,
		history:    make([][]byte, 0, 100),
		historyIdx: -1,
		termWidth:  80,
		termHeight: 24,
		echo:       echo,
	}
}

const (
	KeyCtrlC     = 3
	KeyCtrlD     = 4
	KeyEnter     = '\r'
	KeyEscape    = 27
	KeyBackspace = 127
	KeyUnknown   = 256 + iota
	KeyLeft
	KeyUp
	KeyRight
	KeyDown
	KeyAltLeft
	KeyAltRight
)

// bytesToKey tries to parse a key sequence from b. If successful, it returns
// the key and the remainder of the input. Otherwise it returns -1.
func bytesToKey(b []byte) (int, []byte) {
	if len(b) == 0 {
		return -1, nil
	}

	if b[0] != KeyEscape {
		return int(b[0]), b[1:]
	}

	if len(b) >= 3 && b[0] == KeyEscape && b[1] == '[' {
		switch b[2] {
		case 'A':
			return KeyUp, b[3:]
		case 'B':
			return KeyDown, b[3:]
		case 'C':
			return KeyRight, b[3:]
		case 'D':
			return KeyLeft, b[3:]
		}
	}

	if len(b) >= 6 &&
		b[0] == KeyEscape &&
		b[1] == '[' &&
		b[2] == '1' &&
		b[3] == ';' &&
		b[4] == '3' {
		switch b[5] {
		case 'C':
			return KeyAltRight, b[6:]
		case 'D':
			return KeyAltLeft, b[6:]
		}
	}

	// If we get here then we have a key that we don't recognise, or a
	// partial sequence. It's not clear how one should find the end of a
	// sequence without knowing them all, but it seems that [a-zA-Z] only
	// appears at the end of a sequence.
	for i, c := range b[0:] {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			return KeyUnknown, b[i+1:]
		}
	}

	return -1, b
}

// queue appends data to the end of t.outBuf
func (t *Terminal) queue(data []byte) {
	t.outBuf = append(t.outBuf, data...)
}

var eraseUnderCursor = []byte{' ', KeyEscape, '[', 'D'}
var space = []byte{' '}

func isPrintable(key int) bool {
	return key >= 32 && key < 127
}

// moveCursorToPos appends data to t.outBuf which will move the cursor to the
// given, logical position in the text.
func (t *Terminal) moveCursorToPos(pos int) {
	if !t.echo {
		return
	}

	x := len(t.prompt) + pos
	y := x / t.termWidth
	x = x % t.termWidth

	up := 0
	if y < t.cursorY {
		up = t.cursorY - y
	}

	down := 0
	if y > t.cursorY {
		down = y - t.cursorY
	}

	left := 0
	if x < t.cursorX {
		left = t.cursorX - x
	}

	right := 0
	if x > t.cursorX {
		right = x - t.cursorX
	}

	t.cursorX = x
	t.cursorY = y
	t.move(up, down, left, right)
}

func (t *Terminal) move(up, down, left, right int) {
	movement := make([]byte, 3*(up+down+left+right))
	m := movement
	for i := 0; i < up; i++ {
		m[0] = KeyEscape
		m[1] = '['
		m[2] = 'A'
		m = m[3:]
	}
	for i := 0; i < down; i++ {
		m[0] = KeyEscape
		m[1] = '['
		m[2] = 'B'
		m = m[3:]
	}
	for i := 0; i < left; i++ {
		m[0] = KeyEscape
		m[1] = '['
		m[2] = 'D'
		m = m[3:]
	}
	for i := 0; i < right; i++ {
		m[0] = KeyEscape
		m[1] = '['
		m[2] = 'C'
		m = m[3:]
	}

	t.queue(movement)
}

func (t *Terminal) clearLineToRight() {
	op := []byte{KeyEscape, '[', 'K'}
	t.queue(op)
}

const maxLineLength = 4096

// handleKey processes the given key and, optionally, returns a line of text
// that the user has entered.
func (t *Terminal) handleKey(key int) (line string, ok bool) {
	switch key {
	case KeyBackspace:
		if t.pos == 0 {
			return
		}
		t.pos--
		t.moveCursorToPos(t.pos)

		copy(t.line[t.pos:], t.line[1+t.pos:])
		t.line = t.line[:len(t.line)-1]
		if t.echo {
			t.writeLine(t.line[t.pos:])
		}
		t.queue(eraseUnderCursor)
		t.moveCursorToPos(t.pos)
	case KeyAltLeft:
		// move left by a word.
		if t.pos == 0 {
			return
		}
		t.pos--
		for t.pos > 0 {
			if t.line[t.pos] != ' ' {
				break
			}
			t.pos--
		}
		for t.pos > 0 {
			if t.line[t.pos] == ' ' {
				t.pos++
				break
			}
			t.pos--
		}
		t.moveCursorToPos(t.pos)
	case KeyAltRight:
		// move right by a word.
		for t.pos < len(t.line) {
			if t.line[t.pos] == ' ' {
				break
			}
			t.pos++
		}
		for t.pos < len(t.line) {
			if t.line[t.pos] != ' ' {
				break
			}
			t.pos++
		}
		t.moveCursorToPos(t.pos)
	case KeyLeft:
		if t.pos == 0 {
			return
		}
		t.pos--
		t.moveCursorToPos(t.pos)
	case KeyRight:
		if t.pos == len(t.line) {
			return
		}
		t.pos++
		t.moveCursorToPos(t.pos)
	case KeyUp:
		if len(t.history) == 0 {
			return
		}
		t.historyIdx--
		t.historyIdx = historyIdxValue(t.historyIdx, t.history)

		h := t.history[t.historyIdx]
		newLine := make([]byte, len(h))
		copy(newLine, h)
		newPos := len(newLine)
		if t.echo {
			t.moveCursorToPos(0)
			t.writeLine(newLine)
			for i := len(newLine); i < len(t.line); i++ {
				t.writeLine(space)
			}
			t.moveCursorToPos(newPos)
		}
		t.line = newLine
		t.pos = newPos
		return

	case KeyDown:
		if len(t.history) == 0 {
			return
		}
		newPos := 0
		newLine := []byte{}
		t.historyIdx++
		if t.historyIdx >= len(t.history) {
			t.historyIdx = len(t.history)
		} else {
			t.historyIdx = historyIdxValue(t.historyIdx, t.history)
			h := t.history[t.historyIdx]
			newLine = make([]byte, len(h))
			copy(newLine, h)
			newPos = len(newLine)
			//			fmt.Println("in")
		}
		if t.echo {
			t.moveCursorToPos(0)
			t.writeLine(newLine)
			for i := len(newLine); i < len(t.line); i++ {
				t.writeLine(space)
			}
			t.moveCursorToPos(newPos)
		}
		t.line = newLine
		t.pos = newPos
		return

	case KeyEnter:
		t.moveCursorToPos(len(t.line))
		t.queue([]byte("\r\n"))
		line = string(t.line)
		ok = true
		t.line = t.line[:0]
		t.pos = 0
		t.cursorX = 0
		t.cursorY = 0
		t.maxLine = 0
		t.historyIdx = len(t.history) + 1
	case KeyCtrlD:
		// add 'exit' to the end of the line
		ok = true
		if len(t.line) == 0 {
			if len(t.line) == maxLineLength {
				return
			}
			if len(t.line) == cap(t.line) {
				newLine := make([]byte, len(t.line), 2*(2+len(t.line)))
				copy(newLine, t.line)
				t.line = newLine
			}
			t.line = t.line[:len(t.line)+4]
			copy(t.line[t.pos+4:], t.line[t.pos:])
			t.line[t.pos] = byte('e')
			if t.echo {
				t.writeLine(t.line[t.pos:])
			}
			t.pos++
			t.moveCursorToPos(t.pos)
			t.line[t.pos] = byte('x')
			if t.echo {
				t.writeLine(t.line[t.pos:])
			}
			t.pos++
			t.moveCursorToPos(t.pos)
			t.line[t.pos] = byte('i')
			if t.echo {
				t.writeLine(t.line[t.pos:])
			}
			t.pos++
			t.moveCursorToPos(t.pos)
			t.line[t.pos] = byte('t')
			if t.echo {
				t.writeLine(t.line[t.pos:])
			}
			t.pos++
			t.moveCursorToPos(t.pos)
		}
	case KeyCtrlC:
		// add '^C' to the end of the line
		if len(t.line) == maxLineLength {
			return
		}
		newLine := make([]byte, len(t.line), 2*(2+len(t.line)))
		copy(newLine, t.line)
		t.line = newLine
		t.line = t.line[:len(t.line)+3]
		copy(t.line[t.pos+3:], t.line[t.pos:])
		t.line[t.pos] = byte('^')
		t.pos++
		t.line[t.pos] = byte('C')
		if t.echo {
			t.writeLine(t.line[t.pos-1:])
		}
		t.pos++
		t.moveCursorToPos(t.pos)
		t.queue([]byte("\r\n"))
		t.line = make([]byte, 0)
		t.pos = 0
		t.cursorX = 0
		t.cursorY = 0

	default:
		if t.AutoCompleteCallback != nil {
			t.lock.Unlock()
			newLine, newPos := t.AutoCompleteCallback(t.line, t.pos, key)
			t.lock.Lock()

			if newLine != nil {
				if t.echo {
					t.moveCursorToPos(0)
					t.writeLine(newLine)
					for i := len(newLine); i < len(t.line); i++ {
						t.writeLine(space)
					}
					t.moveCursorToPos(newPos)
				}
				t.line = newLine
				t.pos = newPos
				return
			}
		}
		if !isPrintable(key) {
			return
		}
		if len(t.line) == maxLineLength {
			return
		}
		if len(t.line) == cap(t.line) {
			newLine := make([]byte, len(t.line), 2*(1+len(t.line)))
			copy(newLine, t.line)
			t.line = newLine
		}
		t.line = t.line[:len(t.line)+1]
		copy(t.line[t.pos+1:], t.line[t.pos:])
		t.line[t.pos] = byte(key)
		if t.echo {
			t.writeLine(t.line[t.pos:])
		}
		t.pos++
		t.moveCursorToPos(t.pos)
	}
	return
}

func (t *Terminal) writeLine(line []byte) {
	for len(line) != 0 {
		remainingOnLine := t.termWidth - t.cursorX
		todo := len(line)
		if todo > remainingOnLine {
			todo = remainingOnLine
		}
		t.queue(line[:todo])
		t.cursorX += todo
		line = line[todo:]

		if t.cursorX == t.termWidth {
			t.cursorX = 0
			t.cursorY++
			if t.cursorY > t.maxLine {
				t.maxLine = t.cursorY
			}
		}
	}
}

func (t *Terminal) Write(buf []byte) (n int, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.cursorX == 0 && t.cursorY == 0 {
		// This is the easy case: there's nothing on the screen that we
		// have to move out of the way.
		return t.c.Write(buf)
	}

	// We have a prompt and possibly user input on the screen. We
	// have to clear it first.
	t.move(0 /* up */, 0 /* down */, t.cursorX /* left */, 0 /* right */)
	t.cursorX = 0
	t.clearLineToRight()

	for t.cursorY > 0 {
		t.move(1 /* up */, 0, 0, 0)
		t.cursorY--
		t.clearLineToRight()
	}

	if _, err = t.c.Write(t.outBuf); err != nil {
		return
	}
	t.outBuf = t.outBuf[:0]

	if n, err = t.c.Write(buf); err != nil {
		return
	}

	t.queue([]byte(t.prompt))
	chars := len(t.prompt)
	if t.echo {
		t.queue(t.line)
		chars += len(t.line)
	}
	t.cursorX = chars % t.termWidth
	t.cursorY = chars / t.termWidth
	t.moveCursorToPos(t.pos)

	if _, err = t.c.Write(t.outBuf); err != nil {
		return
	}
	t.outBuf = t.outBuf[:0]
	return
}

// ReadPassword temporarily changes the prompt and reads a password, without
// echo, from the terminal.
func (t *Terminal) ReadPassword(prompt string) (line string, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	oldPrompt := t.prompt
	t.prompt = prompt
	t.echo = false

	line, err = t.readLine()

	t.prompt = oldPrompt
	t.echo = true

	return
}

// ReadLine returns a line of input from the terminal.
func (t *Terminal) ReadLine() (line string, err error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	return t.readLine()
}

func (t *Terminal) readLine() (line string, err error) {
	// t.lock must be held at this point

	if t.cursorX == 0 && t.cursorY == 0 {
		t.writeLine([]byte(t.prompt))
		t.c.Write(t.outBuf)
		t.outBuf = t.outBuf[:0]
	}

	for {
		rest := t.remainder
		lineOk := false
		for !lineOk {
			var key int
			key, rest = bytesToKey(rest)
			if key < 0 {
				break
			}

			line, lineOk = t.handleKey(key)
			if key == KeyCtrlD && lineOk {
				return "", io.EOF
			}
			if key == KeyCtrlC {
				t.remainder = nil
				return "^C", fmt.Errorf("control-c break")
			}
		}
		if len(rest) > 0 {
			n := copy(t.inBuf[:], rest)
			t.remainder = t.inBuf[:n]
		} else {
			t.remainder = nil
		}
		t.c.Write(t.outBuf)
		t.outBuf = t.outBuf[:0]
		if lineOk {
			if t.echo { //&& len(line) > 0 {
				// don't put passwords into history...
				b := []byte(line)
				h := make([]byte, len(b))
				copy(h, b)
				t.history = append(t.history, h)
			}
			return
		}

		// t.remainder is a slice at the beginning of t.inBuf
		// containing a partial key sequence
		readBuf := t.inBuf[len(t.remainder):]
		var n int

		t.lock.Unlock()
		n, err = t.c.Read(readBuf)
		t.lock.Lock()

		if err != nil {
			return "", err
		}

		t.remainder = t.inBuf[:n+len(t.remainder)]
	}
}

// SetPrompt sets the prompt to be used when reading subsequent lines.
func (t *Terminal) SetPrompt(prompt string) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.prompt = prompt
}

func (t *Terminal) SetSize(width, height int) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.termWidth, t.termHeight = width, height
}

func (t *Terminal) SetHistory(h []string) {
	// t.history = make([][]byte, len(h))
	// for i := range h {
	// 	t.history[i] = []byte(h[i])
	// }
	// //	t.historyIdx = len(h)
}

func (t *Terminal) GetHistory() (h []string) {
	// h = make([]string, len(t.history))
	// for i := range t.history {
	// 	h[i] = string(t.history[i])
	// }
	return
}

type shell struct {
	r io.Reader
	w io.Writer
}

func (sh *shell) Read(data []byte) (n int, err error) {
	return sh.r.Read(data)
}

func (sh *shell) Write(data []byte) (n int, err error) {
	return sh.w.Write(data)
}

//var oldState *State

func (t *Terminal) ReleaseFromStdInOut() { // doesn't really need a receiver, but maybe oldState can be part of term one day
	//fd := int(os.Stdin.Fd())
	//Restore(fd, oldState)
}

func NewWithStdInOut(echo bool) (term *Terminal, err error) {
	//fd := int(os.Stdin.Fd())
	//oldState, err = MakeRaw(fd)
	if err != nil {
		panic(err)
	}
	sh := &shell{r: os.Stdin, w: os.Stdout}
	term = NewTerminal(sh, "", echo)
	return
}
