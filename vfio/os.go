//go:build linux

package vfio

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// rawOS is the syscall seam. linuxOS talks to the kernel. Fake, in fake.go,
// answers the same ioctl numbers from process memory.
type rawOS interface {
	Open(path string, flags int, perm uint32) (int, error)
	Close(fd int) error
	IoctlInt(fd int, req uintptr, val uintptr) (uintptr, error)
	IoctlPtr(fd int, req uintptr, arg unsafe.Pointer) (uintptr, error)
	Mmap(fd int, offset int64, length, prot, flags int) ([]byte, error)
	Munmap(b []byte) error
	Pread(fd int, p []byte, off int64) (int, error)
	Pwrite(fd int, p []byte, off int64) (int, error)
	Mlock(b []byte) error
}

type linuxOS struct{}

// Linux is the production syscall backend.
func Linux() rawOS { return linuxOS{} }

func (linuxOS) Open(path string, flags int, perm uint32) (int, error) {
	return unix.Open(path, flags, perm)
}

func (linuxOS) Close(fd int) error { return unix.Close(fd) }

func (linuxOS) IoctlInt(fd int, req uintptr, val uintptr) (uintptr, error) {
	return ioctlSyscall(fd, req, val)
}

func (linuxOS) IoctlPtr(fd int, req uintptr, arg unsafe.Pointer) (uintptr, error) {
	return ioctlSyscall(fd, req, uintptr(arg))
}

func ioctlSyscall(fd int, req, arg uintptr) (uintptr, error) {
	for {
		r, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, arg)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return r, errno
		}
		return r, nil
	}
}

func (linuxOS) Mmap(fd int, offset int64, length, prot, flags int) ([]byte, error) {
	return unix.Mmap(fd, offset, length, prot, flags)
}

func (linuxOS) Munmap(b []byte) error { return unix.Munmap(b) }

func (linuxOS) Pread(fd int, p []byte, off int64) (int, error) {
	return unix.Pread(fd, p, off)
}

func (linuxOS) Pwrite(fd int, p []byte, off int64) (int, error) {
	return unix.Pwrite(fd, p, off)
}

func (linuxOS) Mlock(b []byte) error { return unix.Mlock(b) }
