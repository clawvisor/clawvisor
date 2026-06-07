package stream

import "bytes"

const maxSSELineSize = 4 << 20

func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func discardLastRawLine(buf *bytes.Buffer, line string) {
	n := len(line) + 1
	if n <= 0 || buf.Len() < n {
		return
	}
	buf.Truncate(buf.Len() - n)
}
