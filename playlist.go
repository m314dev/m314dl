package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m314dev/m314dl/internal/engine"
	"github.com/m314dev/m314dl/internal/manifest"
	"github.com/m314dev/m314dl/internal/source"
)

var (
	// errLiveEntry: the entry's manifest turned out live. A playlist run skips
	// it instead of recording until stopped.
	errLiveEntry = errors.New("live stream")
	// errExists: the entry's muxed output is already on disk.
	errExists = errors.New("already downloaded")
)

// downloadPlaylist downloads the selected entries of an IPTV M3U list, -titles
// at a time (default one: an IPTV account allows only a connection or two),
// each to <dir>/<group>/<title>.<ext>. Live channels and titles already on disk
// are skipped, so rerunning the same command resumes the batch. A failed title
// is reported and the rest still run. Titles in flight together share one
// status line (their combined progress); their messages print above it.
func downloadPlaylist(ctx context.Context, cancel context.CancelFunc, j job, pl *manifest.Master) error {
	if j.o.serve != "" {
		return fmt.Errorf("-serve restreams one stream, not an M3U playlist")
	}
	entries, err := selectEntries(pl.Streams, j.o.pick, j.o.group, j.o.match)
	if err != nil {
		return err
	}
	if j.o.listOnly {
		for _, e := range entries {
			live := ""
			if source.LiveEntry(e.PlaylistURL) {
				live = "  (live)"
			}
			fmt.Printf("%5s  [%s] %s%s\n", e.ID, e.GroupID, e.Name, live)
		}
		return nil
	}
	if len(entries) == 0 {
		return fmt.Errorf("no playlist entries matched (%d in the list)", len(pl.Streams))
	}
	dir := j.o.output
	if dir == "" {
		dir = "."
	}
	interruptHandler(false, nil, cancel) // Ctrl-C stops the batch; rerun to resume
	j.skipLive = true
	par := max(1, j.o.titles)
	stopProg := func() {}
	if par > 1 {
		j.prog = engine.NewProgress(false, j.o.progressInterval)
		stop := make(chan struct{})
		go j.prog.Render(stop)
		stopProg = func() {
			close(stop)
			time.Sleep(50 * time.Millisecond) // let renderer print the final line
		}
	}
	var (
		mu            sync.Mutex
		done, skipped int
		failed        []int
		wg            sync.WaitGroup
	)
	slots := make(chan struct{}, par)
	used := map[string]bool{}
dispatch:
	for i, e := range entries {
		tag := fmt.Sprintf("[%d/%d] #%s %s", i+1, len(entries), e.ID, e.Name)
		if source.LiveEntry(e.PlaylistURL) {
			j.say("%s: live channel, skipped", tag)
			mu.Lock()
			skipped++
			mu.Unlock()
			continue
		}
		base := filepath.Join(dir, safeName(e.GroupID, ""), safeName(e.Name, "entry-"+e.ID))
		if used[base] { // two entries with one title in one group
			base += " (" + e.ID + ")"
		}
		used[base] = true
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			j.say("%s", tag)
			err := j.downloadEntry(ctx, cancel, e.PlaylistURL, base)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				done++
			case errors.Is(err, errLiveEntry), errors.Is(err, errExists):
				j.say("%s: %v, skipped", tag, err)
				skipped++
			case ctx.Err() != nil:
			default:
				j.say("%s: failed: %v", tag, err)
				id, _ := strconv.Atoi(e.ID)
				failed = append(failed, id)
			}
		}()
	}
	wg.Wait()
	stopProg()
	fmt.Fprintf(os.Stderr, "playlist: %d downloaded, %d skipped, %d failed\n", done, skipped, len(failed))
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		ids := make([]string, len(failed))
		for i, id := range failed {
			ids[i] = strconv.Itoa(id)
		}
		return fmt.Errorf("%d entries failed; retry them with -pick %s", len(failed), strings.Join(ids, ","))
	}
	return nil
}

// downloadEntry loads one entry and downloads it to base plus the extension
// its kind implies: a direct file keeps its own (.mkv, .mp4, …), a manifest
// is muxed to .mp4.
func (j job) downloadEntry(ctx context.Context, cancel context.CancelFunc, u, base string) error {
	master, kind, err := source.LoadManifest(ctx, j.client, u, j.logv)
	if err != nil {
		return err
	}
	if kind == source.PlaylistKind {
		return fmt.Errorf("entry is itself an M3U playlist")
	}
	out := base + ".mp4"
	if kind == source.FileKind {
		out = base + filepath.Ext(master.Streams[0].Name) // resume and "already complete" are the file path's
	} else if _, err := os.Stat(out); err == nil {
		return errExists // the muxed output only appears once the title is done
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	o := *j.o
	o.output = out
	j.o = &o
	return j.download(ctx, cancel, u, master, kind)
}

// selectEntries applies -pick (entry numbers as -list shows them), -group and
// -match (case-insensitive regexes on group-title and title); all must hold.
// None given selects everything.
func selectEntries(all []*manifest.Stream, pick, group, match string) ([]*manifest.Stream, error) {
	var want map[int]bool
	if pick != "" {
		var err error
		if want, err = parsePick(pick); err != nil {
			return nil, err
		}
	}
	var gre, mre *regexp.Regexp
	for _, f := range []struct {
		expr string
		re   **regexp.Regexp
		name string
	}{{group, &gre, "-group"}, {match, &mre, "-match"}} {
		if f.expr == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + f.expr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
		*f.re = re
	}
	var out []*manifest.Stream
	for i, e := range all {
		if (want != nil && !want[i+1]) ||
			(gre != nil && !gre.MatchString(e.GroupID)) ||
			(mre != nil && !mre.MatchString(e.Name)) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// parsePick parses "3,7-12" into the set {3,7,8,…,12}.
func parsePick(s string) (map[int]bool, error) {
	want := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		b := a
		if err == nil && isRange {
			b, err = strconv.Atoi(hi)
		}
		if err != nil || a < 1 || b < a {
			return nil, fmt.Errorf("bad -pick %q: want entry numbers like 3,7-12", s)
		}
		for n := a; n <= b; n++ {
			want[n] = true
		}
	}
	return want, nil
}

// safeName makes a playlist title or group a file/folder name on every OS:
// characters Windows forbids become '_', trailing dots and spaces (Windows
// drops them) go, and it is capped at 100 characters. Empty gives def.
func safeName(s, def string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100])
	}
	if s = strings.TrimRight(s, ". "); s == "" {
		return def
	}
	return s
}
