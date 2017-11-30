package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/puellanivis/breton/lib/files"
	"github.com/puellanivis/breton/lib/files/httpfiles"
	_ "github.com/puellanivis/breton/lib/files/plugins"
	"github.com/puellanivis/breton/lib/glog"
	flag "github.com/puellanivis/breton/lib/gnuflag"
	"github.com/puellanivis/breton/lib/util"
)

var Flags struct {
	Output    string `flag:",short=o"            desc:"Specifies which file to write the output to"`
	UserAgent string `flag:",default=icycat/1.0" desc:"Which User-Agent string to use"`
	Quiet     bool   `flag:",short=q"            desc:"If set, supresses output from subprocesses."`

	Timeout time.Duration `flag:",default=5s"    desc:"Timeout for each read, if it expires, entire request will restart."`
}

func init() {
	flag.FlagStruct("", &Flags)
}

type Headerer interface {
	Header() (http.Header, error)
}

func PrintIcyHeaders(h Headerer) {
	header, err := h.Header()
	if err != nil {
		glog.Errorf("couldn’t get headers: %s", err)
		return
	}

	var headers []string

	for k, _ := range header {
		key := strings.ToUpper(k)

		if strings.HasPrefix(key, "ICY-") {
			headers = append(headers, k)
		}
	}
	sort.Strings(headers)

	for _, k := range headers {
		v := header[k]

		if len(v) == 1 {
			util.Statusf("%s: %q\n", k, v[0])
		} else {
			util.Statusf("%s: %q\n", k, v)
		}
	}
}

var stderr = os.Stderr

func openOutput(ctx context.Context, filename string) (io.WriteCloser, error) {
	if !strings.HasPrefix(filename, "udp:") {
		return files.Create(ctx, filename)
	}

	ffmpegArgs := []string{
		"-i", "-",
		"-codec", "copy",
		"-copyts",
		"-f", "mpegts",
		"-mpegts_service_type", "digital_radio",
		"-mpegts_copyts", "1",
		"-mpegts_m2ts_mode", "1",
		"-mpegts_flags", "initial_discontinuity",
		"-mpegts_flags", "system_b",
		filename,
	}

	if glog.V(5) {
		glog.Infof("ffmpeg %q", ffmpegArgs)
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs...)
	cmd.Stderr = stderr

	out, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return out, nil
}

func main() {
	defer util.Init("icycat", 1, 0)()

	if glog.V(2) {
		if err := flag.Set("stderrthreshold", "INFO"); err != nil {
			glog.Error(err)
		}
	}

	if Flags.Quiet {
		stderr = nil
	}

	ctx, cancel := context.WithCancel(util.Context())
	defer cancel()

	ctx = httpfiles.WithUserAgent(ctx, Flags.UserAgent)

	args := flag.Args()

	if len(args) < 1 {
		glog.Error("icycat requires an address to stream")
		util.Exit(1)
	}

	out, err := openOutput(ctx, Flags.Output)
	if err != nil {
		glog.Fatal(err)
	}
	defer func() {
		if err := out.Close(); err != nil {
			glog.Error(err)
		}
	}()

	arg, args := args[0], args[1:]

	for {
		select {
		case <-ctx.Done():
			glog.Error(ctx.Err())
			return
		default:
		}

		// BUG: if you attempt to load a SHOUTcast 1.9.x address,
		// it will return an HTTP version field of "ICY" not "HTTP/x.y",
		// and Go’s net/http library will barf and return an error.
		// There is no way at this time to tell it to treat said HTTP version as "HTTP/1.0"
		// without possibly hijacking the stream through a text transform that looks to see if it
		// starts with ICY, and replaces that with HTTP/1.0…
		//
		// BETTER: net/http should allow one to say "ICY" maps to HTTP/1.0,
		// it already has short-circuits for "HTTP/1.0" and "HTTP/1.1" after all.
		f, err := files.Open(ctx, arg)
		if err != nil {
			glog.Fatal(err)
		}

		if fi, err := f.Stat(); err == nil {
			if glog.V(2) && fi.Name() != arg {
				glog.Infof("catting %s", fi.Name())
			}
		}

		if h, ok := f.(Headerer); ok {
			PrintIcyHeaders(h)
		}

		start := time.Now()
		wait := time.After(Flags.Timeout)

		n, err := files.CopyWithRunningTimeout(ctx, out, f, Flags.Timeout)
		if err != nil {
			glog.Error(err)

			if n > 0 {
				glog.Errorf("%d bytes copied in %v", n, time.Since(start))
			}
		}

		// minimum Flags.Timeout wait.
		<-wait

		if glog.V(2) {
			glog.Infof("%d bytes copied in %v", n, time.Since(start))
		}
	}
}
