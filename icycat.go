package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/puellanivis/breton/lib/io/bufpipe"
	"github.com/puellanivis/breton/lib/files"
	"github.com/puellanivis/breton/lib/files/httpfiles"
	_ "github.com/puellanivis/breton/lib/files/plugins"
	_ "github.com/puellanivis/breton/lib/files/udp_noerr"
	"github.com/puellanivis/breton/lib/glog"
	flag "github.com/puellanivis/breton/lib/gnuflag"
	"github.com/puellanivis/breton/lib/metrics"
	_ "github.com/puellanivis/breton/lib/metrics/http"
	"github.com/puellanivis/breton/lib/mpeg/framer"
	"github.com/puellanivis/breton/lib/mpeg/ts"
	"github.com/puellanivis/breton/lib/util"
)

var Flags struct {
	Output     string `flag:",short=o"            desc:"Specifies which file to write the output to"`
	UserAgent  string `flag:",default=icycat/1.0" desc:"Which User-Agent string to use"`
	Quiet      bool   `flag:",short=q"            desc:"If set, supresses output from subprocesses."`
	PacketSize int    `flag:",default=1316"       desc:"Attach a packet size to any ffmpeg udp protocol that is used."`

	Timeout time.Duration `flag:",default=5s"    desc:"Timeout for each read, if it expires, entire request will restart."`

	Metrics        bool   `desc:"If set, publish metrics to the given metrics-port or metrics-addr."`
	MetricsPort    int    `desc:"Which port to publish metrics with. (default auto-assign)"`
	MetricsAddress string `desc:"Which local address to listen on; overrides metrics-port flag."`
}

func init() {
	flag.Struct("", &Flags)
}

var (
	bwSummary = metrics.Summary("icy_read_bandwidth", "a summary of the bandwidth during reading", metrics.CommonObjectives())
)

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

	for _, key := range headers {
		val := header[key]

		var v interface{} = val
		if len(val) == 1 {
			v = val[0]
		}

		util.Statusf("%s: %q\n", key, v)
	}
}

var stderr = os.Stderr

func openOutput(ctx context.Context, filename string) (io.WriteCloser, error) {
	f, err := files.Create(ctx, filename)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(filename, "udp:") {
		return f, nil
	}

	mux := ts.NewMux(f)

	var wg sync.WaitGroup

	wr, err := mux.Writer(ctx, 1, ts.ProgramTypeAudio)
	if err != nil {
		f.Close()
		return nil, err
	}

	pipe := bufpipe.New(ctx)
	s := framer.NewScanner(pipe)

	wg.Add(1)
	go func() {
		defer func() {
			if err := wr.Close(); err != nil {
				glog.Errorf("mux.Writer.Close: %+v", err)
			}

			wg.Done()
		}()

		for s.Scan() {
			b := s.Bytes()

			n, err := wr.Write(b)
			if err != nil {
				glog.Errorf("mux.Writer.Write: %+v", err)
				return
			}

			if n < len(b) {
				glog.Errorf("mux.Writer.Write: %+v", io.ErrShortWrite)
			}
		}

		if err := s.Err(); err != nil {
			glog.Errorf("framer.Scanner: %+v", filename, err)
		}
	}()

	go func() {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		for err := range mux.Serve(ctx) {
			glog.Errorf("mux.Serve: %+v", err)
			cancel()
		}
	}()

	go func() {
		wg.Wait()
		for err := range mux.Close() {
			glog.Errorf("mux.Close: %+v", err)
		}
	}()

	return pipe, nil
}

func main() {
	finish, ctx := util.Init("icycat", 1, 2)
	defer finish()

	ctx = httpfiles.WithUserAgent(ctx, Flags.UserAgent)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	args := flag.Args()
	if len(args) < 1 {
		util.Statusln(flag.Usage)
		util.Exit(1)
	}

	if Flags.Quiet {
		stderr = nil
	}

	if glog.V(2) {
		if err := flag.Set("stderrthreshold", "INFO"); err != nil {
			glog.Error(err)
		}
	}

	if Flags.MetricsPort != 0 || Flags.MetricsAddress != "" {
		Flags.Metrics = true
	}

	if Flags.Metrics {
		go func() {
			addr := Flags.MetricsAddress
			if addr == "" {
				addr = fmt.Sprintf(":%d", Flags.MetricsPort)
			}

			l, err := net.Listen("tcp", addr)
			if err != nil {
				glog.Fatal("net.Listen: ", err)
			}

			msg := fmt.Sprintf("metrics available at: http://%s/metrics/prometheus", l.Addr())
			util.Statusln(msg)
			glog.Info(msg)

			http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
				http.Redirect(w, req, "/metrics/prometheus", http.StatusMovedPermanently)
			})

			srv := &http.Server{}

			go func() {
				<-ctx.Done()
				srv.Shutdown(util.Context())
				l.Close()
			}()

			if err := srv.Serve(l); err != nil {
				if err != http.ErrServerClosed {
					glog.Fatal("http.Serve: ", err)
				}
			}
		}()
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

	var opts []files.CopyOption
	opts = append(opts, files.WithWatchdogTimeout(Flags.Timeout))

	if Flags.Metrics {
		opts = append(opts, files.WithBandwidthMetrics(bwSummary))
	}

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

		n, err := files.Copy(ctx, out, f, opts...)
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
