package agent

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	errShortFrame = errors.New("agent/spool: short frame")
	errBadFrame   = errors.New("agent/spool: bad frame checksum")
)

const frameHeader = 8 // u32 len + u32 crc

// encodeFrame appends one framed record to dst and returns the extended slice.
// frame = [u32 len][u32 crc32c(payload)][payload]; payload = [u64 seq][body].
func encodeFrame(dst []byte, seq int64, body []byte) []byte {
	payloadLen := 8 + len(body)
	var hdr [frameHeader]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(payloadLen))
	// build payload contiguously so the CRC covers seq+body
	payload := make([]byte, payloadLen)
	binary.LittleEndian.PutUint64(payload[0:8], uint64(seq))
	copy(payload[8:], body)
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, crcTable))
	dst = append(dst, hdr[:]...)
	dst = append(dst, payload...)
	return dst
}

// decodeFrame reads one frame from the front of src. It returns errShortFrame
// (with n==0) when src does not yet hold a complete frame — the caller treats
// that as a torn tail — and errBadFrame when the checksum does not match.
func decodeFrame(src []byte) (seq int64, body []byte, n int, err error) {
	if len(src) < frameHeader {
		return 0, nil, 0, errShortFrame
	}
	payloadLen := int(binary.LittleEndian.Uint32(src[0:4]))
	if payloadLen < 8 {
		return 0, nil, 0, errBadFrame
	}
	crc := binary.LittleEndian.Uint32(src[4:8])
	if len(src) < frameHeader+payloadLen {
		return 0, nil, 0, errShortFrame
	}
	payload := src[frameHeader : frameHeader+payloadLen]
	if crc32.Checksum(payload, crcTable) != crc {
		return 0, nil, 0, errBadFrame
	}
	seq = int64(binary.LittleEndian.Uint64(payload[0:8]))
	body = payload[8:]
	return seq, body, frameHeader + payloadLen, nil
}
