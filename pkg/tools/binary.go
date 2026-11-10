package tools

import "bytes"

// BinarySniffLen is the prefix examined for a NUL byte, matching git's
// heuristic and the HTTP file viewer. A later NUL does not by itself mark
// the file as binary.
const BinarySniffLen = 8 * 1024

// IsBinarySniff reports whether data looks like a binary file. The file
// viewer and project search share this rule so a search hit can be opened
// as text.
func IsBinarySniff(data []byte) bool {
	n := len(data)
	if n > BinarySniffLen {
		n = BinarySniffLen
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}
