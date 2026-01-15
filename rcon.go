package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

const (
	ServerDataAuth          int32 = 3
	ServerDataAuthResponse  int32 = 2
	ServerDataExecCommand   int32 = 2
	ServerDataResponseValue int32 = 0
	maxPacketSize                 = 8192 + 12
)

type RCONPacket struct {
	Size int32
	ID   int32
	Type int32
	Body string
}

var currentRequestID int32

func newRequestID() int32 {
	return atomic.AddInt32(&currentRequestID, 1)
}

func encodePacket(id, packetType int32, body string) ([]byte, error) {
	bodyBytes := []byte(body)
	payloadLen := int32(4 + 4 + len(bodyBytes) + 1 + 1)

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, payloadLen); err != nil {
		return nil, fmt.Errorf("编码 RCON 包 - 写入 Size 失败: %w", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, id); err != nil {
		return nil, fmt.Errorf("编码 RCON 包 - 写入 ID 失败: %w", err)
	}
	if err := binary.Write(&buf, binary.LittleEndian, packetType); err != nil {
		return nil, fmt.Errorf("编码 RCON 包 - 写入 Type 失败: %w", err)
	}
	if _, err := buf.Write(bodyBytes); err != nil {
		return nil, fmt.Errorf("编码 RCON 包 - 写入 Body 失败: %w", err)
	}
	if err := buf.WriteByte(0x00); err != nil {
		return nil, fmt.Errorf("编码 RCON 包 - 写入 null 终止符失败: %w", err)
	}
	if err := buf.WriteByte(0x00); err != nil {
		return nil, fmt.Errorf("编码 RCON 包 - 写入第二个 null 终止符失败: %w", err)
	}

	return buf.Bytes(), nil
}

func readPacket(conn net.Conn) (*RCONPacket, error) {
	var payloadSize int32
	if err := binary.Read(conn, binary.LittleEndian, &payloadSize); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("读取 RCON 包大小失败: %w", err)
	}

	if payloadSize < 10 || payloadSize > (maxPacketSize-4) {
		return nil, fmt.Errorf("RCON 包大小无效: %d", payloadSize)
	}

	packetData := make([]byte, payloadSize)
	if _, err := io.ReadFull(conn, packetData); err != nil {
		return nil, fmt.Errorf("读取 RCON 包数据体失败: %w", err)
	}

	var id, packetType int32
	buf := bytes.NewReader(packetData)

	if err := binary.Read(buf, binary.LittleEndian, &id); err != nil {
		return nil, fmt.Errorf("解码 RCON 包 ID 失败: %w", err)
	}
	if err := binary.Read(buf, binary.LittleEndian, &packetType); err != nil {
		return nil, fmt.Errorf("解码 RCON 包 Type 失败: %w", err)
	}

	var bodyBuffer bytes.Buffer
	for {
		b, err := buf.ReadByte()
		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("RCON 包 Body 未以 null 结尾")
			}
			return nil, fmt.Errorf("读取 RCON 包 Body 字节失败: %w", err)
		}
		if b == 0x00 {
			break
		}
		bodyBuffer.WriteByte(b)
	}
	body := bodyBuffer.String()

	b, err := buf.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("读取第二个 null 终止符失败: %w", err)
	}
	if b != 0x00 {
		return nil, fmt.Errorf("第二个 null 终止符格式错误: 0x%x", b)
	}

	return &RCONPacket{
		Size: payloadSize + 4,
		ID:   id,
		Type: packetType,
		Body: body,
	}, nil
}

func Authenticate(conn net.Conn, password string, authRequestID int32) error {
	LogDebug("正在尝试 RCON 认证...")
	authPacket, err := encodePacket(authRequestID, ServerDataAuth, password)
	if err != nil {
		return fmt.Errorf("编码认证包失败: %w", err)
	}

	if _, err := conn.Write(authPacket); err != nil {
		return fmt.Errorf("发送认证包失败: %w", err)
	}

	for i := 0; i < 2; i++ {
		respPacket, err := readPacket(conn)
		if err != nil {
			return fmt.Errorf("读取认证响应失败: %w", err)
		}

		if respPacket.Type == ServerDataAuthResponse {
			if respPacket.ID == authRequestID {
				LogDebug("RCON 认证成功")
				return nil
			}
			if respPacket.ID == -1 {
				return fmt.Errorf("RCON 认证失败 (服务器返回 ID: -1)")
			}
		}
		if respPacket.Type == ServerDataResponseValue {
			LogDebug("收到预认证包 [ID: %d, Type: %d]", respPacket.ID, respPacket.Type)
			continue
		}
		return fmt.Errorf("认证过程中收到意外类型的包")
	}
	return fmt.Errorf("RCON 认证超时")
}

func SendCommand(conn net.Conn, command string) error {
	reqID := newRequestID()
	cmdPacket, err := encodePacket(reqID, ServerDataExecCommand, command)
	if err != nil {
		return fmt.Errorf("编码命令包失败: %w", err)
	}

	if _, err := conn.Write(cmdPacket); err != nil {
		return fmt.Errorf("发送命令包失败: %w", err)
	}
	LogDebug("已发送命令 (ID: %d): %s", reqID, command)
	return nil
}
