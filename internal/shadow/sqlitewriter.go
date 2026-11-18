// Package shadow provides passive audit: it simulates the tuple transformation
// on real traffic without touching it, logs a summary and appends rows to a
// dependency-free SQLite 3 database file.
package shadow

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
)

// Kind is a SQLite storage class.
type Kind int

const (
	// KindNull stores SQL NULL.
	KindNull Kind = iota
	// KindInt stores an INTEGER.
	KindInt
	// KindFloat stores a REAL.
	KindFloat
	// KindText stores TEXT (UTF-8).
	KindText
	// KindBlob stores BLOB.
	KindBlob
)

// Value is one column value.
type Value struct {
	Kind Kind
	I    int64
	F    float64
	T    string
	B    []byte
}

// Int builds an INTEGER value.
func Int(v int64) Value { return Value{Kind: KindInt, I: v} }

// Float builds a REAL value.
func Float(v float64) Value { return Value{Kind: KindFloat, F: v} }

// Text builds a TEXT value.
func Text(v string) Value { return Value{Kind: KindText, T: v} }

// Blob builds a BLOB value.
func Blob(v []byte) Value { return Value{Kind: KindBlob, B: v} }

// Null builds a NULL value.
func Null() Value { return Value{Kind: KindNull} }

const (
	magic            = "SQLite format 3\x00"
	defaultPageSize  = 4096
	schemaTableSQL   = "CREATE TABLE shadow_records (ts TEXT, path TEXT, mode TEXT, tokens_before INTEGER, tokens_after INTEGER, output_savings_pct REAL, net_savings_pct REAL, latency_us INTEGER, activated INTEGER)"
	schemaRootPage   = 2
	leafPageType     = 0x0d
	interiorPageType = 0x05
	columnCount      = 9
)

// Writer appends rows to a pure-Go SQLite 3 file. Rows are buffered and the
// whole database is laid out when Close is called.
type Writer struct {
	path     string
	pageSize int

	mu        sync.Mutex
	rows      [][]Value
	nextRowID uint64
	closed    bool
}

// NewWriter creates (or truncates) a database file with the schema and an empty
// row page.
func NewWriter(path string, pageSize int) (*Writer, error) {
	if path == "" {
		return nil, errors.New("shadow: database path is required")
	}
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return nil, fmt.Errorf("shadow: page size must be a power of two between 512 and 65536, got %d", pageSize)
	}

	w := &Writer{path: path, pageSize: pageSize}
	if err := w.writeFile(); err != nil {
		return nil, err
	}
	return w, nil
}

// Insert queues one row. Rows are written to disk on Close.
func (w *Writer) Insert(columns []Value) error {
	if len(columns) != columnCount {
		return fmt.Errorf("shadow: expected %d columns, got %d", columnCount, len(columns))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("shadow: writer is closed")
	}
	row := make([]Value, len(columns))
	copy(row, columns)
	w.rows = append(w.rows, row)
	return nil
}

// Close lays out every page and flushes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.writeFile()
}

// pageNode is one b-tree page waiting to be laid out.
type pageNode struct {
	leaf     bool
	cells    [][]byte
	children []*pageNode
	maxRow   uint64
	page     int
}

// writeFile renders the complete database. Callers hold the lock.
func (w *Writer) writeFile() error {
	leaves, err := w.buildLeaves()
	if err != nil {
		return err
	}

	// Build interior levels until a single root remains.
	level := leaves
	for len(level) > 1 {
		level = groupNodes(w.pageSize, level)
	}
	root := level[0]

	// Page 2 is the root; children are numbered breadth-first from 3.
	next := 3
	root.page = schemaRootPage
	var assign func(*pageNode)
	assign = func(n *pageNode) {
		for _, child := range n.children {
			child.page = next
			next++
			assign(child)
		}
	}
	assign(root)

	totalPages := next - 1
	pages := make([][]byte, totalPages+1)

	master := make([]byte, w.pageSize)
	writeDatabaseHeader(master, w.pageSize, totalPages)
	masterRecord := buildRecord([]Value{
		Text("table"),
		Text("shadow_records"),
		Text("shadow_records"),
		Int(schemaRootPage),
		Text(schemaTableSQL),
	})
	masterCell := append(varint(uint64(len(masterRecord))), varint(1)...)
	masterCell = append(masterCell, masterRecord...)
	writeLeafPage(master, 100, [][]byte{masterCell})
	pages[1] = master

	var render func(*pageNode) error
	render = func(n *pageNode) error {
		buf := make([]byte, w.pageSize)
		if n.leaf {
			writeLeafPage(buf, 0, n.cells)
		} else {
			if err := writeInteriorPage(buf, 0, n.children); err != nil {
				return err
			}
		}
		pages[n.page] = buf
		for _, child := range n.children {
			if err := render(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := render(root); err != nil {
		return err
	}

	file := make([]byte, 0, w.pageSize*totalPages)
	for i := 1; i <= totalPages; i++ {
		file = append(file, pages[i]...)
	}
	return os.WriteFile(w.path, file, 0o600)
}

// buildLeaves turns buffered rows into leaf pages.
func (w *Writer) buildLeaves() ([]*pageNode, error) {
	leaves := []*pageNode{}
	current := &pageNode{leaf: true}
	used := 0

	for index, row := range w.rows {
		rowID := uint64(index + 1)
		record := buildRecord(row)
		cell := append(varint(uint64(len(record))), varint(rowID)...)
		cell = append(cell, record...)

		cellSize := len(cell) + 2 // pointer
		if cellSize > w.pageSize-8 {
			return nil, fmt.Errorf("shadow: row %d is larger than a page", index+1)
		}
		if used+cellSize > w.pageSize-8 && len(current.cells) > 0 {
			leaves = append(leaves, current)
			current = &pageNode{leaf: true}
			used = 0
		}
		current.cells = append(current.cells, cell)
		used += cellSize
		if rowID > current.maxRow {
			current.maxRow = rowID
		}
	}
	leaves = append(leaves, current)
	return leaves, nil
}

// groupNodes packs nodes into interior pages until the page budget fits.
func groupNodes(pageSize int, nodes []*pageNode) []*pageNode {
	var out []*pageNode
	current := &pageNode{}
	used := 0
	for _, child := range nodes {
		cellSize := 4 + varintLen(child.maxRow) + 2
		if used+cellSize > pageSize-12 && len(current.children) > 0 {
			out = append(out, current)
			current = &pageNode{}
			used = 0
		}
		current.children = append(current.children, child)
		if child.maxRow > current.maxRow {
			current.maxRow = child.maxRow
		}
		used += cellSize
	}
	out = append(out, current)
	return out
}

func writeDatabaseHeader(page []byte, pageSize, pageCount int) {
	copy(page, magic)
	binary.BigEndian.PutUint16(page[16:], uint16(pageSize))
	page[18] = 1 // file format write version
	page[19] = 1 // file format read version
	page[20] = 0 // reserved bytes per page
	page[21] = 64
	page[22] = 32
	page[23] = 32
	binary.BigEndian.PutUint32(page[24:], 1) // file change counter
	binary.BigEndian.PutUint32(page[28:], uint32(pageCount))
	binary.BigEndian.PutUint32(page[32:], 0) // freelist head
	binary.BigEndian.PutUint32(page[36:], 0) // freelist pages
	binary.BigEndian.PutUint32(page[40:], 1) // schema cookie
	binary.BigEndian.PutUint32(page[44:], 4) // schema format number
	binary.BigEndian.PutUint32(page[48:], 0) // default page cache
	binary.BigEndian.PutUint32(page[52:], 0) // largest root b-tree page
	binary.BigEndian.PutUint32(page[56:], 1) // UTF-8
}

func writeLeafPage(page []byte, offset int, cells [][]byte) {
	header := page[offset:]
	header[0] = leafPageType
	binary.BigEndian.PutUint16(header[1:], 0) // first freeblock
	binary.BigEndian.PutUint16(header[3:], uint16(len(cells)))
	header[7] = 0 // fragmented free bytes

	contentStart := len(page)
	pointerBase := offset + 8
	for i, cell := range cells {
		contentStart -= len(cell)
		copy(page[contentStart:], cell)
		binary.BigEndian.PutUint16(page[pointerBase+2*i:], uint16(contentStart))
	}
	if len(cells) == 0 {
		contentStart = len(page)
	}
	binary.BigEndian.PutUint16(header[5:], uint16(contentStart))
}

// writeInteriorPage lays out an interior table page.
func writeInteriorPage(page []byte, offset int, children []*pageNode) error {
	header := page[offset:]
	header[0] = interiorPageType
	binary.BigEndian.PutUint16(header[1:], 0)
	binary.BigEndian.PutUint16(header[3:], uint16(maxInt(0, len(children)-1)))
	header[7] = 0
	if len(children) == 0 {
		binary.BigEndian.PutUint16(header[5:], uint16(len(page)))
		return nil
	}
	binary.BigEndian.PutUint32(header[8:], uint32(children[len(children)-1].page))

	contentStart := len(page)
	pointerBase := offset + 12
	for i := 0; i < len(children)-1; i++ {
		cell := make([]byte, 4)
		binary.BigEndian.PutUint32(cell, uint32(children[i].page))
		cell = append(cell, varint(children[i].maxRow)...)
		contentStart -= len(cell)
		if contentStart < pointerBase+2*(len(children)-1) {
			return errors.New("shadow: interior page overflow")
		}
		copy(page[contentStart:], cell)
		binary.BigEndian.PutUint16(page[pointerBase+2*i:], uint16(contentStart))
	}
	binary.BigEndian.PutUint16(header[5:], uint16(contentStart))
	return nil
}

// buildRecord encodes values in SQLite record format (header of serial types
// followed by the bodies).
func buildRecord(values []Value) []byte {
	types := make([]byte, 0, len(values))
	bodies := make([]byte, 0, 64)
	for _, value := range values {
		serial, body := encodeValue(value)
		types = append(types, varint(serial)...)
		bodies = append(bodies, body...)
	}

	// The header stores its own size first: solve H = len(types) + varintLen(H).
	headerSize := len(types) + 1
	for varintLen(uint64(headerSize)) != headerSize-len(types) {
		headerSize = len(types) + varintLen(uint64(headerSize))
	}

	out := make([]byte, 0, headerSize+len(bodies))
	out = append(out, varint(uint64(headerSize))...)
	out = append(out, types...)
	out = append(out, bodies...)
	return out
}

func encodeValue(value Value) (uint64, []byte) {
	switch value.Kind {
	case KindNull:
		return 0, nil
	case KindInt:
		return encodeInteger(value.I)
	case KindFloat:
		body := make([]byte, 8)
		binary.BigEndian.PutUint64(body, math.Float64bits(value.F))
		return 7, body
	case KindText:
		raw := []byte(value.T)
		return uint64(13 + 2*len(raw)), raw
	case KindBlob:
		return uint64(12 + 2*len(value.B)), value.B
	default:
		return 0, nil
	}
}

func encodeInteger(value int64) (uint64, []byte) {
	switch {
	case value == 0:
		return 8, nil
	case value > 0 && value <= 127:
		return 1, []byte{byte(value)}
	case value >= -128 && value < 0:
		return 1, []byte{byte(value)}
	case value > 0 && value <= 32767:
		body := make([]byte, 2)
		binary.BigEndian.PutUint16(body, uint16(value))
		return 2, body
	case value >= -32768 && value < 0:
		body := make([]byte, 2)
		binary.BigEndian.PutUint16(body, uint16(value))
		return 2, body
	case value > 0 && value <= 8388607:
		body := make([]byte, 3)
		body[0] = byte(value >> 16)
		body[1] = byte(value >> 8)
		body[2] = byte(value)
		return 3, body
	case value >= -8388608 && value < 0:
		body := make([]byte, 3)
		body[0] = byte(uint32(value) >> 16)
		body[1] = byte(value >> 8)
		body[2] = byte(value)
		return 3, body
	case value > 0 && value <= 2147483647:
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body, uint32(value))
		return 4, body
	case value >= -2147483648 && value < 0:
		body := make([]byte, 4)
		binary.BigEndian.PutUint32(body, uint32(value))
		return 4, body
	default:
		body := make([]byte, 8)
		binary.BigEndian.PutUint64(body, uint64(value))
		return 6, body
	}
}

// varint encodes a SQLite variable-length integer (1-9 bytes, big-endian).
func varint(value uint64) []byte {
	if value <= 0x7f {
		return []byte{byte(value)}
	}
	if value > 0x00ffffffffffffff {
		out := make([]byte, 9)
		out[8] = byte(value)
		value >>= 8
		for i := 7; i >= 0; i-- {
			out[i] = byte(value) | 0x80
			value >>= 7
		}
		return out
	}
	length := 0
	for temp := value; temp > 0; temp >>= 7 {
		length++
	}
	out := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		out[i] = byte(value & 0x7f)
		if i < length-1 {
			out[i] |= 0x80
		}
		value >>= 7
	}
	return out
}

func varintLen(value uint64) int {
	switch {
	case value <= 0x7f:
		return 1
	case value <= 0x3fff:
		return 2
	case value <= 0x1fffff:
		return 3
	case value <= 0xfffffff:
		return 4
	case value <= 0x7ffffffff:
		return 5
	case value <= 0x3ffffffffff:
		return 6
	case value <= 0x1ffffffffffff:
		return 7
	case value <= 0xffffffffffffff:
		return 8
	default:
		return 9
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
