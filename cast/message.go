package cast

import (
	"encoding/binary"
	"fmt"
	"io"
)

// castMessage is the internal representation of a Cast protocol message.
type castMessage struct {
	sourceID  string
	destID    string
	namespace string
	payload   string
}

// writeMessage encodes msg as a length-prefixed protobuf frame and writes it to w.
func writeMessage(w io.Writer, msg castMessage) error {
	enc := protoEncode(msg)
	if err := binary.Write(w, binary.BigEndian, uint32(len(enc))); err != nil {
		return err
	}
	_, err := w.Write(enc)
	return err
}

// readMessage reads one length-prefixed protobuf frame from r.
func readMessage(r io.Reader) (castMessage, error) {
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return castMessage{}, err
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return castMessage{}, err
	}
	return protoDecode(data)
}

// protoEncode encodes a CastMessage using the Cast v2 protobuf wire format.
//
// CastMessage fields used:
//
//	1  protocol_version  varint  = 0 (CASTV2_1_0)
//	2  source_id         string
//	3  destination_id    string
//	4  namespace         string
//	5  payload_type      varint  = 0 (STRING)
//	6  payload_utf8      string
func protoEncode(m castMessage) []byte {
	var b []byte
	b = appendVarintField(b, 1, 0) // protocol_version = CASTV2_1_0
	b = appendStringField(b, 2, m.sourceID)
	b = appendStringField(b, 3, m.destID)
	b = appendStringField(b, 4, m.namespace)
	b = appendVarintField(b, 5, 0) // payload_type = STRING
	b = appendStringField(b, 6, m.payload)
	return b
}

// protoDecode decodes a CastMessage from raw protobuf bytes.
// Only fields 2–4 and 6 (source, dest, namespace, payload_utf8) are extracted.
func protoDecode(data []byte) (castMessage, error) {
	var m castMessage
	i := 0
	for i < len(data) {
		tag, n := decodeVarint(data[i:])
		if n == 0 {
			return m, fmt.Errorf("bad varint at byte %d", i)
		}
		i += n
		fieldNum := int(tag >> 3)
		wireType := tag & 0x7

		switch wireType {
		case 0: // varint — read and discard
			_, n = decodeVarint(data[i:])
			if n == 0 {
				return m, fmt.Errorf("bad varint value at byte %d", i)
			}
			i += n
		case 2: // length-delimited (string/bytes)
			length, n := decodeVarint(data[i:])
			if n == 0 {
				return m, fmt.Errorf("bad length varint at byte %d", i)
			}
			i += n
			end := i + int(length)
			if end > len(data) {
				return m, fmt.Errorf("field %d extends past end of data", fieldNum)
			}
			val := string(data[i:end])
			i = end
			switch fieldNum {
			case 2:
				m.sourceID = val
			case 3:
				m.destID = val
			case 4:
				m.namespace = val
			case 6:
				m.payload = val
			}
		default:
			return m, fmt.Errorf("unexpected wire type %d for field %d", wireType, fieldNum)
		}
	}
	return m, nil
}

func appendVarintField(b []byte, fieldNum int, val uint64) []byte {
	b = appendVarint(b, uint64(fieldNum<<3|0)) // wire type 0 = varint
	return appendVarint(b, val)
}

func appendStringField(b []byte, fieldNum int, s string) []byte {
	b = appendVarint(b, uint64(fieldNum<<3|2)) // wire type 2 = length-delimited
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func decodeVarint(b []byte) (uint64, int) {
	var val uint64
	for i, byt := range b {
		val |= uint64(byt&0x7f) << (7 * uint(i))
		if byt < 0x80 {
			return val, i + 1
		}
		if i >= 9 {
			return 0, 0
		}
	}
	return 0, 0
}
