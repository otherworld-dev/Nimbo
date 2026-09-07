//go:build windows

// Package overlay hosts the local IPC endpoint that the Windows Explorer shell
// extension queries for per-file sync status, and the notification used to make
// Explorer refresh an overlay when a file's state changes.
package overlay

import (
	"bufio"
	"context"
	"net"
	"strings"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// PipeName is the named pipe the shell extension connects to. It must match the
// name compiled into the overlay DLL.
const PipeName = `\\.\pipe\Nimbo-overlay`

// requestPrefix is the line protocol the shell extension uses:
//
//	RETRIEVE_FILE_STATUS:<absolute-path>\n   (request)
//	STATUS:<STATE>:<absolute-path>\n         (reply; STATE = OK|SYNC|WARN|NONE)
const requestPrefix = "RETRIEVE_FILE_STATUS:"

// Server answers shell-overlay status queries over a named pipe.
type Server struct {
	ln net.Listener
}

// Serve starts the pipe server. statusFn maps an absolute path to a state
// string ("ok"/"sync"/"warn"/"none"). The server runs until ctx is cancelled.
func Serve(ctx context.Context, statusFn func(string) string) (*Server, error) {
	ln, err := winio.ListenPipe(PipeName, &winio.PipeConfig{
		// Grant access to authenticated users so explorer.exe (same user) can
		// connect regardless of integrity-level quirks.
		SecurityDescriptor: "D:P(A;;GA;;;AU)",
	})
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go s.accept(statusFn)
	return s, nil
}

func (s *Server) accept(statusFn func(string) string) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go handle(conn, statusFn)
	}
}

func handle(conn net.Conn, statusFn func(string) string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, requestPrefix) {
			continue
		}
		path := line[len(requestPrefix):]
		state := strings.ToUpper(statusFn(path))
		if _, err := w.WriteString("STATUS:" + state + ":" + path + "\n"); err != nil {
			return
		}
		if w.Flush() != nil {
			return
		}
	}
}

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procSHChangeNotify = shell32.NewProc("SHChangeNotify")
)

const (
	shcneUpdateItem = 0x00002000 // SHCNE_UPDATEITEM
	shcnfPathW      = 0x0005     // SHCNF_PATHW
)

// NotifyChange asks Explorer to refresh the display (and thus the overlay icon)
// for absPath. Safe to call from any goroutine; a no-op on error.
func NotifyChange(absPath string) {
	p, err := windows.UTF16PtrFromString(absPath)
	if err != nil {
		return
	}
	_, _, _ = procSHChangeNotify.Call(uintptr(shcneUpdateItem), uintptr(shcnfPathW), uintptr(unsafe.Pointer(p)), 0)
}
