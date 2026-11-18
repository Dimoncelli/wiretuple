package shadow

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// --- minimal SQLite reader used only by tests -----------------------------

func readVarint(data []byte) (uint64, int) {
	var value uint64
	for i := 0; i < 9 && i < len(data); i++ {
		b := data[i]
		if i == 8 {
			value = (value << 8) | uint64(b)
			return value, 9
		}
		value = (value << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			return value, i + 1
		}
	}
	return value, len(data)
}

func readDB(t *testing.T, path string) [][]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if len(raw) < 100 {
		t.Fatal("database too short")
	}
	if !bytes.Equal(raw[:16], []byte(magic)) {
		t.Fatalf("bad magic: %q", raw[:16])
	}
	pageSize := int(binary.BigEndian.Uint16(raw[16:]))
	if pageSize == 0 {
		t.Fatal("page size is zero")
	}
	page := func(n int) []byte {
		start := (n - 1) * pageSize
		if start+pageSize > len(raw) {
			t.Fatalf("page %d out of range", n)
		}
		return raw[start : start+pageSize]
	}

	// sqlite_master lives on page 1 after the 100-byte file header.
	master := page(1)
	masterCells := decodeLeafCells(master, 100)
	if len(masterCells) == 0 {
		t.Fatal("sqlite_master has no rows")
	}
	masterRow := decodeRecord(t, masterCells[0])
	if len(masterRow) < 5 {
		t.Fatalf("sqlite_master row too short: %v", masterRow)
	}
	if masterRow[1] != "shadow_records" {
		t.Fatalf("table name = %v", masterRow[1])
	}
	if sql, _ := masterRow[4].(string); !bytes.Contains([]byte(sql), []byte("CREATE TABLE shadow_records")) {
		t.Fatalf("schema sql = %v", masterRow[4])
	}

	// Walk the table b-tree from its root page.
	var rows [][]any
	var walk func(pageNum int)
	walk = func(pageNum int) {
		current := page(pageNum)
		switch current[0] {
		case leafPageType:
			for _, cell := range decodeLeafCells(current, 0) {
				rows = append(rows, decodeRecord(t, cell))
			}
		case interiorPageType:
			count := int(binary.BigEndian.Uint16(current[3:]))
			rightMost := int(binary.BigEndian.Uint32(current[8:]))
			var children []int
			for i := 0; i < count; i++ {
				offset := int(binary.BigEndian.Uint16(current[12+2*i:]))
				children = append(children, int(binary.BigEndian.Uint32(current[offset:])))
			}
			children = append(children, rightMost)
			for _, child := range children {
				walk(child)
			}
		default:
			t.Fatalf("page %d has unknown type %#x", pageNum, current[0])
		}
	}
	walk(schemaRootPage)

	return rows
}

func decodeLeafCells(page []byte, headerOffset int) [][]byte {
	header := page[headerOffset:]
	count := int(binary.BigEndian.Uint16(header[3:]))
	cells := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		offset := int(binary.BigEndian.Uint16(page[headerOffset+8+2*i:]))
		payloadLen, n := readVarint(page[offset:])
		_, m := readVarint(page[offset+n:])
		start := offset + n + m
		cells = append(cells, page[start:start+int(payloadLen)])
	}
	return cells
}

func decodeRecord(t *testing.T, record []byte) []any {
	t.Helper()
	headerSize, n := readVarint(record)
	if int(headerSize) > len(record) {
		t.Fatalf("header size %d beyond record", headerSize)
	}
	var serials []uint64
	pos := n
	for pos < int(headerSize) {
		serial, size := readVarint(record[pos:])
		serials = append(serials, serial)
		pos += size
	}

	values := make([]any, 0, len(serials))
	body := int(headerSize)
	for _, serial := range serials {
		switch {
		case serial == 0:
			values = append(values, nil)
		case serial >= 1 && serial <= 6:
			length := []int{0, 1, 2, 3, 4, 6, 8}[serial]
			var integer int64
			switch length {
			case 1:
				integer = int64(int8(record[body]))
			case 2:
				integer = int64(int16(binary.BigEndian.Uint16(record[body:])))
			case 3:
				value := int32(record[body])<<16 | int32(record[body+1])<<8 | int32(record[body+2])
				if value&0x800000 != 0 {
					value |= ^0xffffff
				}
				integer = int64(value)
			case 4:
				integer = int64(int32(binary.BigEndian.Uint32(record[body:])))
			case 6:
				value := int64(binary.BigEndian.Uint32(record[body:])) << 16
				value |= int64(binary.BigEndian.Uint16(record[body+4:]))
				integer = value
			case 8:
				integer = int64(binary.BigEndian.Uint64(record[body:]))
			}
			values = append(values, integer)
			body += length
		case serial == 7:
			values = append(values, math.Float64frombits(binary.BigEndian.Uint64(record[body:])))
			body += 8
		case serial == 8:
			values = append(values, int64(0))
		case serial == 9:
			values = append(values, int64(1))
		case serial >= 12 && serial%2 == 0:
			length := int((serial - 12) / 2)
			values = append(values, append([]byte(nil), record[body:body+length]...))
			body += length
		case serial >= 13:
			length := int((serial - 13) / 2)
			values = append(values, string(record[body:body+length]))
			body += length
		default:
			t.Fatalf("unknown serial type %d", serial)
		}
	}
	return values
}

// --- tests ----------------------------------------------------------------

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "shadow.db")
}

func TestNewWriterCreatesValidHeader(t *testing.T) {
	path := tempDB(t)
	writer, err := NewWriter(path, 4096)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte(magic)) {
		t.Fatalf("missing SQLite magic: %q", raw[:16])
	}
	if size := int(binary.BigEndian.Uint16(raw[16:])); size != 4096 {
		t.Fatalf("page size = %d", size)
	}
	rows := readDB(t, path)
	if len(rows) != 0 {
		t.Fatalf("expected an empty table, got %d rows", len(rows))
	}
}

func TestWriterRoundTripsStorageClasses(t *testing.T) {
	path := tempDB(t)
	writer, err := NewWriter(path, 512)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}

	err = writer.Insert([]Value{
		Text("2026-01-02T03:04:05Z"),
		Text("/v1/chat/completions"),
		Text(`mode "quoted" ✨`),
		Int(-42),
		Int(0),
		Float(4.8),
		Float(-0.5),
		Int(1234567),
		Int(1),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	err = writer.Insert([]Value{
		Text("2026-01-02T03:04:06Z"),
		Text("/v1/messages"),
		Text("passthrough"),
		Int(10),
		Int(10),
		Float(0),
		Float(0),
		Int(0),
		Int(0),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rows := readDB(t, path)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	first := rows[0]
	if first[2] != `mode "quoted" ✨` {
		t.Fatalf("utf8/quotes mangled: %v", first[2])
	}
	if first[3] != int64(-42) {
		t.Fatalf("negative integer = %v", first[3])
	}
	if first[5] != 4.8 {
		t.Fatalf("real = %v", first[5])
	}
	if first[7] != int64(1234567) {
		t.Fatalf("latency = %v", first[7])
	}
}

func TestWriterSpansManyPages(t *testing.T) {
	path := tempDB(t)
	writer, err := NewWriter(path, 512)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	const total = 500
	for i := 0; i < total; i++ {
		err := writer.Insert([]Value{
			Text("2026-01-02T03:04:05Z"),
			Text("/v1/chat/completions"),
			Text("activated"),
			Int(int64(i)),
			Int(0),
			Float(42.5),
			Float(40.1),
			Int(int64(i * 10)),
			Int(1),
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rows := readDB(t, path)
	if len(rows) != total {
		t.Fatalf("rows = %d, want %d", len(rows), total)
	}
	if got := rows[total-1][3]; got != int64(total-1) {
		t.Fatalf("last row value = %v", got)
	}
}

func TestWriterRejectsBadPageSize(t *testing.T) {
	if _, err := NewWriter(tempDB(t), 1000); err == nil {
		t.Fatal("expected page size rejection")
	}
	if _, err := NewWriter("", 0); err == nil {
		t.Fatal("expected path rejection")
	}
}

func TestWriterInsertAfterCloseFails(t *testing.T) {
	writer, err := NewWriter(tempDB(t), 512)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := writer.Insert(make([]Value, columnCount)); err == nil {
		t.Fatal("expected insert after close to fail")
	}
}

func TestVarintRoundTrip(t *testing.T) {
	values := []uint64{0, 1, 127, 128, 16383, 16384, 1 << 20, 1 << 35, math.MaxUint32}
	for _, value := range values {
		encoded := varint(value)
		decoded, size := readVarint(encoded)
		if decoded != value {
			t.Fatalf("varint(%d) decoded as %d", value, decoded)
		}
		if size != len(encoded) {
			t.Fatalf("varint(%d) size = %d, want %d", value, size, len(encoded))
		}
	}
	_ = fmt.Sprintf("")
}
