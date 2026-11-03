package binary

import (
	"encoding/binary"
	"testing"

	"github.com/stones-hub/taurus-pro-tcp/pkg/tcp/errors"
)

func header(dataLen uint32) []byte {
	b := make([]byte, HeaderSize)
	binary.BigEndian.PutUint16(b[0:2], MagicNumber)
	b[2] = 1
	binary.BigEndian.PutUint32(b[3:7], 1)
	binary.BigEndian.PutUint32(b[7:11], 1)
	binary.BigEndian.PutUint32(b[11:15], dataLen)
	return b
}

// DataLen 接近 2^32 时，总长度转成 uint32 会回绕。修复前会绕过上限检查、一直等待；修复后按超限处理。
func TestUnpackRejectsLengthThatWrapsUint32(t *testing.T) {
	p := New(1024)
	for _, dataLen := range []uint32{
		0xFFFFFFFF - HeaderSize - FooterSize + 1, // 总长度 == 2^32，回绕为 0
		0xFFFFFFFF,
		0xFFFFFFFF - 2,
		0x80000000, // 不回绕的大长度（对照）
	} {
		data := append(header(dataLen), 0, 0, 0, 0)
		_, n, err := p.Unpack(data)
		if err != errors.ErrMessageTooLarge || n != len(data) {
			t.Fatalf("dataLen %#x: Unpack = (%d, %v), want (%d, ErrMessageTooLarge)", dataLen, n, err, len(data))
		}
	}
}

// 边界：总长度恰好等于上限可以打包和解析，多 1 字节就超限。
func TestPackUnpackLengthBoundary(t *testing.T) {
	msg := &Message{Version: 1, Type: 2, Sequence: 3, Data: []byte("payload")}
	size := HeaderSize + len(msg.Data) + FooterSize

	p := New(uint32(size))
	frame, err := p.Pack(msg)
	if err != nil {
		t.Fatalf("Pack at exactly max size: %v", err)
	}
	if _, n, err := p.Unpack(frame); err != nil || n != size {
		t.Fatalf("Unpack at exactly max size = (%d, %v), want (%d, nil)", n, err, size)
	}

	p = New(uint32(size - 1))
	if _, err := p.Pack(msg); err != errors.ErrMessageTooLarge {
		t.Fatalf("Pack one byte over max = %v, want ErrMessageTooLarge", err)
	}
	if _, n, err := p.Unpack(frame); err != errors.ErrMessageTooLarge || n != size {
		t.Fatalf("Unpack one byte over max = (%d, %v), want (%d, ErrMessageTooLarge)", n, err, size)
	}
}
