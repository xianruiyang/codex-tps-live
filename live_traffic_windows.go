//go:build windows

package codextps

import (
	"encoding/binary"
	"net"
	"strconv"
	"syscall"
	"unsafe"
)

type liveTrafficOwner struct {
	Process string
	PID     uint32
}

func liveTrafficSocketKey(ip, port string) string {
	parsed := net.ParseIP(ip)
	if parsed != nil {
		ip = parsed.String()
	}
	return net.JoinHostPort(ip, port)
}

var trafficTCPTable = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")
var trafficProcessImage = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")

// Read existing owner-PID tables only. GetPerTcpConnectionEStats/ETW would
// require enabling collection or privileges; neither is used here.
func liveTrafficOwners(connections []liveTrafficWireConnection) (map[string]liveTrafficOwner, string) {
	out := map[string]liveTrafficOwner{}
	wanted := map[string]bool{}
	for _, c := range connections {
		port := liveTrafficPort(c.Metadata.SourcePort)
		if port != "" && port != "0" && c.Metadata.Network == "tcp" {
			wanted[liveTrafficSocketKey(c.Metadata.SourceIP, port)] = true
		}
	}
	if len(wanted) == 0 {
		return out, ""
	}
	processes := map[uint32]string{}
	failed := false
	for _, family := range []uintptr{2, 23} {
		var size uint32
		code, _, _ := trafficTCPTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, family, 5, 0)
		if code != 122 && code != 0 {
			failed = true
			continue
		}
		if size < 4 || size > 16<<20 {
			failed = true
			continue
		}
		var table []byte
		for attempt := 0; attempt < 3; attempt++ {
			table = make([]byte, size)
			code, _, _ = trafficTCPTable.Call(uintptr(unsafe.Pointer(&table[0])), uintptr(unsafe.Pointer(&size)), 0, family, 5, 0)
			if code != 122 {
				break
			}
		}
		if code != 0 {
			failed = true
			continue
		}
		rowSize, portOffset, pidOffset := 24, 8, 20
		if family == 23 {
			rowSize, portOffset, pidOffset = 56, 20, 52
		}
		count := int(binary.LittleEndian.Uint32(table[:4]))
		if count > (len(table)-4)/rowSize {
			failed = true
			continue
		}
		for i := 0; i < count; i++ {
			row := table[4+i*rowSize : 4+(i+1)*rowSize]
			var ip net.IP
			if family == 2 {
				ip = net.IP(row[4:8])
			} else {
				ip = net.IP(row[:16])
			}
			key := liveTrafficSocketKey(ip.String(), strconv.Itoa(int(binary.BigEndian.Uint16(row[portOffset:portOffset+2]))))
			if !wanted[key] {
				continue
			}
			pid := binary.LittleEndian.Uint32(row[pidOffset : pidOffset+4])
			name, known := processes[pid]
			if !known {
				if handle, err := syscall.OpenProcess(0x1000, false, pid); err == nil {
					buf := make([]uint16, 32768)
					n := uint32(len(buf))
					ok, _, _ := trafficProcessImage.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
					syscall.CloseHandle(handle)
					if ok != 0 {
						name = proxyProcess(syscall.UTF16ToString(buf[:n]))
					}
				}
				processes[pid] = name
			}
			if name != "" {
				out[key] = liveTrafficOwner{Process: name, PID: pid}
			}
		}
	}
	if failed {
		return out, "部分 Windows 连接归属无法读取；优先使用代理已有进程字段，无法确认的不计入 Codex"
	}
	return out, ""
}
