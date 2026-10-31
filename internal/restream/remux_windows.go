//go:build windows

package restream

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// A Windows child inherits only stdin, stdout and stderr, and Go refuses
// cmd.ExtraFiles there, so the fd 3, 4 pipes of the Unix build cannot exist.
// Each track is a named pipe instead. FFmpeg opens \\.\pipe\<name> like a
// file and its C runtime reports it as a pipe, so FFmpeg streams it the same
// way it streams stdin. Nothing is inherited and nothing goes through the
// network stack, which some machines filter even for loopback.
//
// kernel32 is always loaded in a Windows process, so looking it up by name
// cannot load a planted DLL.
var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateNamedPipeW = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe = kernel32.NewProc("ConnectNamedPipe")
	pipeSeq              atomic.Uint64 // keeps names unique across the channels of one worker process
)

const (
	pipeAccessOutbound      = 0x00000002 // this process writes, FFmpeg reads
	pipeTypeByteBlocking    = 0x00000000 // PIPE_TYPE_BYTE, PIPE_READMODE_BYTE and PIPE_WAIT are all zero
	pipeRejectRemoteClients = 0x00000008
	pipeOutBufferSize       = 1 << 20
	errorPipeConnected      = syscall.Errno(535) // ERROR_PIPE_CONNECTED: the client got there first
)

// namedPipe is the server end of one track's pipe. FFmpeg opens its inputs
// one at a time and probes each before opening the next, so the client end of
// track 1 does not exist until track 0 has delivered enough to be understood.
// The first Write therefore waits for FFmpeg to open its end. Each track runs
// in its own goroutine upstream, so that wait never holds up another track.
type namedPipe struct {
	name   string
	f      *os.File
	once   sync.Once
	err    error
	closed atomic.Bool
}

func newTrackInputs(n int, _ *exec.Cmd) ([]trackInput, func(), error) {
	var tracks []trackInput
	for i := 0; i < n; i++ {
		p, err := newNamedPipe()
		if err != nil {
			for _, t := range tracks {
				t.w.Close()
			}
			return nil, nil, err
		}
		// -seekable 0 tells the file protocol to treat the pipe as a stream
		// even if the runtime's stat did not flag it as one.
		tracks = append(tracks, trackInput{w: p, args: []string{"-seekable", "0", "-i", p.name}})
	}
	return tracks, func() {}, nil
}

func newNamedPipe() (*namedPipe, error) {
	name := fmt.Sprintf(`\\.\pipe\m314dl-%d-%d`, os.Getpid(), pipeSeq.Add(1))
	wname, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, _, e := procCreateNamedPipeW.Call(
		uintptr(unsafe.Pointer(wname)),
		pipeAccessOutbound,
		pipeTypeByteBlocking|pipeRejectRemoteClients,
		1, // one instance: one FFmpeg input
		pipeOutBufferSize,
		0, // no inbound buffer, FFmpeg never writes back
		0, // default wait timeout, unused here
		0, // default security: not inheritable, this user only
	)
	if syscall.Handle(h) == syscall.InvalidHandle {
		return nil, fmt.Errorf("remux: create named pipe: %w", e)
	}
	return &namedPipe{name: name, f: os.NewFile(h, name)}, nil
}

// connect waits for the client end to open. It runs once, from the first
// Write. ERROR_PIPE_CONNECTED means FFmpeg opened the pipe before this call
// and is not an error.
func (p *namedPipe) connect() {
	r, _, e := procConnectNamedPipe.Call(p.f.Fd(), 0)
	if r == 0 && e != errorPipeConnected {
		p.err = e
	}
	if p.closed.Load() {
		p.err = os.ErrClosed
	}
}

func (p *namedPipe) Write(b []byte) (int, error) {
	p.once.Do(p.connect)
	if p.err != nil {
		return 0, p.err
	}
	return p.f.Write(b)
}

// Close ends the track. A Write still waiting in connect is freed by opening
// the client end from here for a moment; it then sees closed and fails. When
// FFmpeg already holds the client end that open fails, which is fine. Data
// FFmpeg has not read yet stays in the pipe until it reads it, then it gets
// EOF, the same as closing the write end of an anonymous pipe.
func (p *namedPipe) Close() error {
	p.closed.Store(true)
	if c, err := os.Open(p.name); err == nil {
		c.Close()
	}
	p.once.Do(func() { p.err = os.ErrClosed })
	return p.f.Close()
}
