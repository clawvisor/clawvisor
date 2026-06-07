package stream

import "bytes"

const maxSSELineSize = 16 << 20

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
	for _, suffix := range [][]byte{
		[]byte(line + "\n"),
		[]byte(line),
	} {
		if len(suffix) == 0 || buf.Len() < len(suffix) {
			continue
		}
		raw := buf.Bytes()
		if bytes.Equal(raw[len(raw)-len(suffix):], suffix) {
			buf.Truncate(buf.Len() - len(suffix))
			return
		}
	}
}
