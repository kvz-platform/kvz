package runtime

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
)

// ReadCRILog serves `kubectl logs` from a CRI-format log file
// ("<RFC3339Nano> <stream> <F|P> <msg>") honoring tail, since, timestamps,
// limit-bytes, previous (path+".1") and follow. Lines that are not in CRI
// format (older runtimes) pass through verbatim.
func ReadCRILog(ctx context.Context, path string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	if opts.Previous {
		path += ".1"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var since time.Time
	if !opts.SinceTime.IsZero() {
		since = opts.SinceTime
	} else if opts.SinceSeconds > 0 {
		since = time.Now().Add(-time.Duration(opts.SinceSeconds) * time.Second)
	}
	format := func(line string) (string, bool) {
		ts, rest, ok := strings.Cut(line, " ")
		if !ok {
			return line + "\n", true
		}
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return line + "\n", true
		}
		if !since.IsZero() && t.Before(since) {
			return "", false
		}
		_, rest, _ = strings.Cut(rest, " ") // stream
		tag, msg, _ := strings.Cut(rest, " ")
		if tag != "P" {
			msg += "\n"
		}
		if opts.Timestamps {
			msg = ts + " " + msg
		}
		return msg, true
	}
	// Initial content (tail applies here).
	var out bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var lines []string
	for sc.Scan() {
		if s, ok := format(sc.Text()); ok {
			lines = append(lines, s)
		}
	}
	if opts.Tail > 0 && len(lines) > opts.Tail {
		lines = lines[len(lines)-opts.Tail:]
	}
	for _, l := range lines {
		out.WriteString(l)
	}
	if opts.LimitBytes > 0 && out.Len() > opts.LimitBytes {
		out.Truncate(opts.LimitBytes)
	}
	if !opts.Follow {
		f.Close()
		return io.NopCloser(&out), nil
	}
	pr, pw := io.Pipe()
	go func() {
		defer f.Close()
		if _, err := pw.Write(out.Bytes()); err != nil {
			return
		}
		br := bufio.NewReader(f)
		var partial string
		for {
			chunk, err := br.ReadString('\n')
			if err == nil {
				if s, ok := format(strings.TrimSuffix(partial+chunk, "\n")); ok {
					if _, werr := pw.Write([]byte(s)); werr != nil {
						return
					}
				}
				partial = ""
				continue
			}
			partial += chunk
			select {
			case <-ctx.Done():
				pw.Close()
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return pr, nil
}
