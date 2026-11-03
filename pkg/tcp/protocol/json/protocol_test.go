package json

import (
	"encoding/binary"
	"testing"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
)

func header(dataLen uint32) []byte {
	b := make([]byte, HeaderSize)
	binary.BigEndian.PutUint16(b[0:2], MagicNumber)
	binary.BigEndian.PutUint32(b[2:6], dataLen)
	return b
}

// 头部长度接近 2^32 时，总长度转成 uint32 会回绕成很小的值。修复前这样的帧会绕过上限检查，
// 被当作"数据不足"一直等待；修复后按超限处理并丢弃当前数据。
func TestUnpackRejectsLengthThatWrapsUint32(t *testing.T) {
	p := New(1024)
	for _, dataLen := range []uint32{
		0xFFFFFFFF - HeaderSize + 1, // HeaderSize+dataLen == 2^32，回绕为 0
		0xFFFFFFFF,                  // 回绕为 HeaderSize-1
		0xFFFFFFFF - 2,              // 回绕为 HeaderSize-3
		0x80000000,                  // 不回绕的大长度（对照）
	} {
		data := append(header(dataLen), []byte("{}")...)
		_, n, err := p.Unpack(data)
		if err != errors.ErrMessageTooLarge || n != len(data) {
			t.Fatalf("dataLen %#x: Unpack = (%d, %v), want (%d, ErrMessageTooLarge)", dataLen, n, err, len(data))
		}
	}
}

// 边界：总长度恰好等于上限的帧可以解析，多 1 字节就超限。
func TestUnpackLengthBoundary(t *testing.T) {
	body := []byte(`{"type":1,"sequence":1,"data":{},"timestamp":1}`)
	frame := append(header(uint32(len(body))), body...)

	p := New(uint32(len(frame)))
	if _, n, err := p.Unpack(frame); err != nil || n != len(frame) {
		t.Fatalf("frame of exactly max size: Unpack = (%d, %v), want (%d, nil)", n, err, len(frame))
	}
	p = New(uint32(len(frame) - 1))
	if _, n, err := p.Unpack(frame); err != errors.ErrMessageTooLarge || n != len(frame) {
		t.Fatalf("frame one byte over max: Unpack = (%d, %v), want (%d, ErrMessageTooLarge)", n, err, len(frame))
	}
}

// Pack 边界：总长度恰好等于上限可以打包，多 1 字节就超限；打包结果可以原样解析回来。
func TestPackLengthBoundary(t *testing.T) {
	msg := &Message{Type: 1, Sequence: 1, Data: map[string]interface{}{}, Timestamp: 1}
	frame, err := New(1 << 20).Pack(msg)
	if err != nil {
		t.Fatal(err)
	}
	size := len(frame)

	p := New(uint32(size))
	got, err := p.Pack(msg)
	if err != nil || len(got) != size {
		t.Fatalf("Pack at exactly max size = (%d bytes, %v), want (%d, nil)", len(got), err, size)
	}
	if m, n, err := p.Unpack(got); err != nil || n != size || m.(*Message).Sequence != 1 {
		t.Fatalf("Unpack of packed frame = (%v, %d, %v)", m, n, err)
	}
	if _, err := New(uint32(size - 1)).Pack(msg); err != errors.ErrMessageTooLarge {
		t.Fatalf("Pack one byte over max = %v, want ErrMessageTooLarge", err)
	}
}
