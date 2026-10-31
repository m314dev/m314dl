//go:build !windows

package restream

import (
	"fmt"
	"os"
	"os/exec"
)

// newTrackInputs makes one anonymous pipe per track. FFmpeg inherits the read
// ends as fd 3, 4 and so on and reads them as pipe:3, pipe:4 and so on. The
// returned release closes this process's copies of the read ends. It must run
// after the child has started, so that an input reaches EOF once its write
// end is closed and the child is the only reader left.
func newTrackInputs(n int, cmd *exec.Cmd) ([]trackInput, func(), error) {
	var tracks []trackInput
	closeReadEnds := func() {
		for _, r := range cmd.ExtraFiles {
			r.Close()
		}
	}
	for i := 0; i < n; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			for _, t := range tracks {
				t.w.Close()
			}
			closeReadEnds()
			return nil, nil, err
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, r) // entry i becomes fd 3+i in the child
		tracks = append(tracks, trackInput{w: w, args: []string{"-i", fmt.Sprintf("pipe:%d", 3+i)}})
	}
	return tracks, closeReadEnds, nil
}
