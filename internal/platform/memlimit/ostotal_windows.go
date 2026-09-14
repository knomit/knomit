//go:build windows

package memlimit

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX struct. x/sys/windows does
// not wrap GlobalMemoryStatusEx, so the call is made through kernel32 directly.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// osTotal reports physical RAM. Windows has no cgroups, so this is the only
// source; a failure here yields SourceNone and a fixed-default fallback rather
// than an error, because embeddings are mandatory.
func osTotal() (int64, error) {
	st := memoryStatusEx{}
	st.Length = uint32(unsafe.Sizeof(st))
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0, err
	}
	return int64(st.TotalPhys), nil
}
